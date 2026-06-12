package main

import (
	"github.com/RobertMe/gocec"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
)

func init() {
	RegisterInitializer(0, InitAcitveSourceBridge)
}

type ActiveSourceBridge struct {
	cec                 *Cec
	mqtt                *Mqtt
	activeSource        *Device
	devices             *DeviceRegistry
	monitor             *Monitor
	allowedSources      map[gocec.LogicalAddress]bool
	haBridge            *HomeAssistantBridge
	lastPhysicalAddress string

	// stateMutex guards lastPhysicalAddress, claimCount, routeTimer,
	// routeTarget, routingNewerThanClaim and tvPoweredOn: these are touched
	// from libcec's callback goroutine AND from time.AfterFunc goroutines
	// (routed publish, power-on requests).
	stateMutex  sync.Mutex
	claimCount  uint64
	routeTimer  *time.Timer
	tvPoweredOn bool

	// routeTarget is the pending routed publish's address; only meaningful
	// while routeTimer != nil. A claim for a DIFFERENT address while a routed
	// target is pending is a stale answer to an outdated routing request and
	// is ignored (v0.0.5): 2026-06-12 19:01 a resting PS5, woken by the user's
	// remote merely hopping THROUGH HDMI1 on the way to HDMI3, claimed 1.0.0.0
	// 5 s after the user had landed on 3 — cancelling the pending 3.0.0.0 and
	// freezing the topic on the wrong input with no later frame to correct it.
	routeTarget gocec.PhysicalAddress

	// routingNewerThanClaim is true when the latest routing frame postdates
	// the latest accepted claim/standby. While true, checkActiveSource() must
	// not publish libcec's view: libcec's active source is claim-derived, so
	// after a suppressed stale claim it would resurrect the very value the
	// claim handler just refused (the monitor re-runs ~10 s after each frame
	// and again on its short cycle, outliving the 12 s routed window).
	routingNewerThanClaim bool
}

// Routed publishes must outlast the slowest legitimate claimer (PS5 waking
// from rest: 10.6 s measured 2026-06-12) but stay under the HA-side 15 s
// "TV" debounce hold. SSP re-arms ~0.6 s after RC → worst case 12.6 s.
const routedPublishDelay = 12 * time.Second

func InitAcitveSourceBridge(container *Container) {
	cec := container.Get("cec").(*Cec)
	mqtt := container.Get("mqtt").(*Mqtt)
	devices := container.Get("devices").(*DeviceRegistry)

	bridge := &ActiveSourceBridge{
		cec:            cec,
		mqtt:           mqtt,
		activeSource:   nil,
		devices:        devices,
		allowedSources: make(map[gocec.LogicalAddress]bool),
	}

	bridge.monitor = CreateMonitor(
		func() {},
		bridge.checkActiveSource,
		10*time.Minute,
		10*time.Second,
		1*time.Minute,
	)

	devices.RegisterDeviceAddedHandler(func(device *Device) {
		bridge.allowedSources[device.LogicalAddress] = true
	})

	if haBridge, ok := container.Get("home-assistant").(*HomeAssistantBridge); ok {
		log.Info("Enabling Home Assistant configuration for active source")
		bridge.haBridge = haBridge
		devices.RegisterDeviceAddedHandler(func(device *Device) {
			haBridge.RegisterBinarySensor(device, "is_active_source")
		})
		haBridge.RegisterSensor("active_source", "active_source", mqtt.BuildBridgeTopic("active_source/physical_address"))
		haBridge.RegisterBirthHandler(bridge.resendAll)
	}

	cec.RegisterMessageHandler(func(message gocec.Message) {
		log.WithFields(log.Fields{
			"message.source":      message.Source(),
			"message.destination": message.Destination(),
			"message.opcode":      message.Opcode(),
			"message.raw":         []byte(message),
		}).Debug("Restarting active source monitor")

		bridge.monitor.Reset()
		// RoutingChange included so that switching to the TV's own tuner (which
		// the TV signals via <Routing Change> earlier and more reliably than its
		// laggy <Active Source>) promptly re-runs the authoritative check below —
		// without publishing the raw routing PA (which may be a phantom port).
	}, gocec.OpcodeActiveSource, gocec.OpcodeSetStreamPath, gocec.OpcodeRoutingChange)

	// Immediate fast path for deliberate source switches. Publish ONLY on
	// <Active Source> (0x82): a device broadcasts it when it actually becomes
	// the active source, carrying its own physical address. Unlike
	// <Routing Change> (0x80) / <Set Stream Path> (0x86) — which sweep through
	// and target physical addresses that may point at EMPTY HDMI ports — it is
	// never emitted for a port with no device, so it carries no phantom address
	// (e.g. the spurious 3.0.0.0 / 4.0.0.0 our empty HDMI3 / HDMI4 produced,
	// which froze the retained topic on the wrong source). checkActiveSource()
	// below is the authoritative settle/correction backstop.
	cec.RegisterMessageHandler(func(message gocec.Message) {
		params := message.Parameters()
		if len(params) < 2 {
			return
		}
		claimed := gocec.PhysicalAddress{params[0], params[1]}

		bridge.stateMutex.Lock()
		defer bridge.stateMutex.Unlock()
		bridge.claimCount++

		// Stale-claim guard (v0.0.5): a pending routed target is NEWER user
		// intent than this claim. A device answering an earlier routing hop
		// (PS5 wake-claim: 5-11 s late) must not yank the topic to a port the
		// TV already left, nor kill the routed publish. A claim MATCHING the
		// pending target is its confirmation — publish early, as before. With
		// no routing pending (couch One-Touch Play, tuner announce, power-on
		// request answers) every claim still publishes immediately.
		if bridge.routeTimer != nil && claimed != bridge.routeTarget {
			log.WithFields(log.Fields{
				"claim.physical_address": claimed.String(),
				"route.target":           bridge.routeTarget.String(),
			}).Info("Ignoring active-source claim contradicting newer routing target")
			return
		}

		if bridge.routeTimer != nil {
			bridge.routeTimer.Stop()
			bridge.routeTimer = nil
		}
		bridge.routingNewerThanClaim = false
		bridge.publishPhysicalAddressLocked(claimed)
	}, gocec.OpcodeActiveSource)

	cec.RegisterMessageHandler(func(message gocec.Message) {
		if message.Source() == gocec.DeviceTV {
			bridge.stateMutex.Lock()
			defer bridge.stateMutex.Unlock()
			bridge.claimCount++
			if bridge.routeTimer != nil {
				bridge.routeTimer.Stop()
				bridge.routeTimer = nil
			}
			// TV off is unambiguous — it outranks any pending routing.
			bridge.routingNewerThanClaim = false
			bridge.publishPhysicalAddressLocked(gocec.PhysicalAddress{0x00, 0x00})
		}
	}, gocec.OpcodeStandby, gocec.OpcodeInactiveSource)

	// Delayed-trust routing (v0.0.4): the TV's <Routing Change> /
	// <Set Stream Path> target PA was correct in 8/8 live captures
	// (2026-06-12: NRC + remote, ports 1-4, PC on/off) — including CEC-mute
	// HDMI3 (PC) and empty HDMI4, which can never claim. Devices that CAN
	// claim do so well within the delay (PS5 from rest 10.6 s, awake 0.7 s,
	// our own adapter instantly) and cancel the timer via the handlers above;
	// if nobody claims, the routed target IS the truth. This restores
	// claimless-port visibility without reintroducing b258818's phantoms:
	// a phantom can no longer freeze the topic, because every later RC/SSP
	// re-arms and every claim/standby cancels.
	cec.RegisterMessageHandler(func(message gocec.Message) {
		params := message.Parameters()
		var target gocec.PhysicalAddress
		switch message.Opcode() {
		case gocec.OpcodeRoutingChange:
			if len(params) < 4 {
				return
			}
			target = gocec.PhysicalAddress{params[2], params[3]}
		case gocec.OpcodeSetStreamPath:
			if len(params) < 2 {
				return
			}
			target = gocec.PhysicalAddress{params[0], params[1]}
		}

		// The TV claims its tuner explicitly via <Active Source> (S3/S9);
		// 0.0.0.0 must never be route-published — mid-switch "TV" is exactly
		// the transient this design removes. It still cancels a pending routed
		// publish (v0.0.5): routing toward the tuner is newer intent than the
		// pending port, and without the cancel the stale-claim guard would
		// suppress the TV's genuine tuner announce on a quick port→tuner hop.
		if target == (gocec.PhysicalAddress{0x00, 0x00}) {
			bridge.stateMutex.Lock()
			defer bridge.stateMutex.Unlock()
			bridge.routingNewerThanClaim = true
			if bridge.routeTimer != nil {
				bridge.routeTimer.Stop()
				bridge.routeTimer = nil
				log.Debug("Cancelling pending routed publish: newer routing targets the TV tuner")
			}
			return
		}

		bridge.armRoutedPublish(target)
	}, gocec.OpcodeRoutingChange, gocec.OpcodeSetStreamPath)

	cec.RegisterMessageHandler(func(message gocec.Message) {
		log.WithFields(log.Fields{
			"message.source":      message.Source(),
			"message.destination": message.Destination(),
			"message.opcode":      message.Opcode(),
			"message.raw":         []byte(message),
		}).Debug("Updating active source based on power status")

		defer bridge.monitor.Reset()

		powerStatus := gocec.PowerStatus(message.Parameters()[0])
		bridge.allowedSources[message.Source()] = powerStatus != gocec.PowerStatusStandBy

		if message.Source() == gocec.DeviceTV {
			bridge.handleTvPowerStatus(powerStatus)
		}

		if bridge.activeSource == nil || message.Source() != bridge.activeSource.LogicalAddress {
			return
		}

		if powerStatus == gocec.PowerStatusStandBy {
			bridge.updateActiveSource(nil)
		}
	}, gocec.OpcodeReportPowerStatus)

	cec.RegisterMessageHandler(func(message gocec.Message) {
		if message.Source() == gocec.DeviceTV {
			log.Debug("Setting active source to nil because TV is in standby")
			bridge.stateMutex.Lock()
			bridge.tvPoweredOn = false
			bridge.stateMutex.Unlock()
			bridge.updateActiveSource(nil)
		}
	}, gocec.OpcodeStandby)

	mqtt.RegisterConnectedHandler(bridge.resendAll)

	container.Register("active-source", bridge)
}

func (bridge *ActiveSourceBridge) updateActiveSource(newSource *Device) {
	if bridge.activeSource != nil && newSource == bridge.activeSource {
		return
	}

	if newSource == nil && bridge.activeSource == nil {
		return
	}

	if newSource != nil {
		if allowed, ok := bridge.allowedSources[newSource.LogicalAddress]; ok && !allowed {
			if bridge.activeSource == nil {
				log.WithFields(log.Fields{
					"device.id":              newSource.Id,
					"device.logical_address": newSource.LogicalAddress,
				}).Trace("Skipping active source update because active device still is in standby")

				return
			}

			newSource = nil
		}
	}

	from, to := "", ""
	if bridge.activeSource != nil {
		from = bridge.activeSource.Id
	}
	if newSource != nil {
		to = newSource.Id
	}

	log.WithFields(log.Fields{
		"from": from,
		"to":   to,
	}).Info("Updating active source")

	mqtt := bridge.mqtt
	if bridge.activeSource != nil {
		log.WithFields(log.Fields{
			"device.id": bridge.activeSource.Id,
		}).Debug("Setting device as inactive source")

		mqtt.Publish(mqtt.BuildTopic(bridge.activeSource, "is_active_source"), 0, true, "off")
	}

	if newSource != nil {
		log.WithFields(log.Fields{
			"device.id": newSource.Id,
		}).Debug("Setting device as active source")

		mqtt.Publish(mqtt.BuildTopic(newSource, "is_active_source"), 0, true, "on")
	}

	bridge.activeSource = newSource
}

func (bridge *ActiveSourceBridge) publishPhysicalAddress(address gocec.PhysicalAddress) {
	bridge.stateMutex.Lock()
	defer bridge.stateMutex.Unlock()
	bridge.publishPhysicalAddressLocked(address)
}

// Callers must hold stateMutex.
func (bridge *ActiveSourceBridge) publishPhysicalAddressLocked(address gocec.PhysicalAddress) {
	value := address.String()
	if value == bridge.lastPhysicalAddress {
		return
	}
	bridge.lastPhysicalAddress = value

	log.WithFields(log.Fields{
		"active_source.physical_address": value,
	}).Info("Publishing active source physical address")

	bridge.mqtt.Publish(bridge.mqtt.BuildBridgeTopic("active_source/physical_address"), 0, true, value)
}

func (bridge *ActiveSourceBridge) armRoutedPublish(target gocec.PhysicalAddress) {
	bridge.stateMutex.Lock()
	defer bridge.stateMutex.Unlock()

	if bridge.routeTimer != nil {
		bridge.routeTimer.Stop()
	}
	bridge.routeTarget = target
	bridge.routingNewerThanClaim = true

	log.WithFields(log.Fields{
		"route.target": target.String(),
	}).Debug("Arming delayed routed-source publish")

	bridge.routeTimer = time.AfterFunc(routedPublishDelay, func() {
		bridge.stateMutex.Lock()
		defer bridge.stateMutex.Unlock()

		bridge.routeTimer = nil

		log.WithFields(log.Fields{
			"route.target": target.String(),
		}).Info("No active-source claim after routing change; publishing routed target")

		bridge.publishPhysicalAddressLocked(target)
	})
}

// R1 fix (v0.0.4): the TV announces NOTHING when it powers on to its tuner
// (S1 2026-06-12: 3 power cycles, zero source frames). It DOES answer a
// broadcast <Request Active Source> — within 180 ms when settled, by ~13 s
// during boot (Step 0b / S1) — but only if the request comes from our
// REGISTERED logical address (requests from unregistered LAs are ignored;
// Step 0b probed both). Three staggered requests cover the boot window; each
// is skipped once any claim (or standby) has been seen since scheduling.
func (bridge *ActiveSourceBridge) handleTvPowerStatus(status gocec.PowerStatus) {
	on := status == gocec.PowerStatusOn || status == gocec.PowerStatusTransitionToOn

	bridge.stateMutex.Lock()
	wasOn := bridge.tvPoweredOn
	bridge.tvPoweredOn = on
	claimsBefore := bridge.claimCount
	bridge.stateMutex.Unlock()

	if !on || wasOn {
		return
	}

	adapterAddress, err := bridge.cec.connection.GetAdapterAddress()
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Warn("Cannot request active source: no adapter address")
		return
	}

	request := gocec.NewMessage(adapterAddress, gocec.DeviceBroadcast, gocec.OpcodeRequestActiveSource, []byte{})

	for _, delay := range []time.Duration{8 * time.Second, 16 * time.Second, 24 * time.Second} {
		time.AfterFunc(delay, func() {
			bridge.stateMutex.Lock()
			claimed := bridge.claimCount != claimsBefore
			bridge.stateMutex.Unlock()

			if claimed {
				return
			}

			log.Info("Requesting active source after TV power-on")
			bridge.cec.Transmit(request)
		})
	}
}

func (bridge *ActiveSourceBridge) checkActiveSource() {
	address := bridge.cec.connection.GetActiveSource()

	// Authoritative source of truth. libcec tracks the real active source on the
	// bus; GetPhysicalAddress resolves the physical address for ANY logical
	// address it knows — including devices our DeviceRegistry never matched
	// (e.g. the PS5 on HDMI1) — so it covers every populated input. Publishing it
	// here re-asserts the true source after a switch storm settles (the monitor
	// re-runs on a short timer for ~1 min after each ActiveSource / SetStreamPath
	// / RoutingChange), overwriting any transient value the fast path may have
	// left on the retained topic. 0xFFFF is libcec's "unknown" — never publish it.
	if address != gocec.DeviceUnknown {
		pa := bridge.cec.connection.GetPhysicalAddress(address)

		bridge.stateMutex.Lock()
		routingNewer := bridge.routingNewerThanClaim
		bridge.stateMutex.Unlock()

		// 0xFFFF is libcec's "unknown" — never publish it. 0.0.0.0 (TV/tuner)
		// is likewise excluded here (v0.0.4): libcec's "TV is active" view is
		// stale mid-switch (R2's transient) and would overwrite a routed
		// publish (R3 fix). Tuner truth comes ONLY from explicit TV frames:
		// <Active Source 0.0.0.0>, <Standby>, <Inactive Source> — and the TV
		// reliably sends those (S3/S9/S10).
		//
		// routingNewer (v0.0.5): libcec derives its active source from claims,
		// so while the latest routing postdates the latest accepted claim this
		// view is the SAME stale answer the claim handler suppressed — and the
		// monitor re-runs long past the 12 s routed window, so publishing here
		// would overwrite the routed truth minutes later. The next accepted
		// claim re-enables the monitor's re-assert role.
		if !routingNewer && pa != (gocec.PhysicalAddress{0xFF, 0xFF}) && pa != (gocec.PhysicalAddress{0x00, 0x00}) {
			bridge.publishPhysicalAddress(pa)
		}
	}

	var newSource *Device = nil
	newSourceId := ""
	if address != gocec.DeviceUnknown {
		if newSource = bridge.devices.FindByLogicalAddress(address); newSource != nil {
			newSourceId = newSource.Id
		}
	}

	log.WithFields(log.Fields{
		"active_source.logical_address": address,
		"active_source.id":              newSourceId,
	}).Trace("Updating active source from monitor")

	bridge.updateActiveSource(newSource)
}

func (bridge *ActiveSourceBridge) resendAll() {
	log.Debug("Resending all active source states")
	for _, device := range bridge.devices.List() {
		if bridge.haBridge != nil {
			bridge.haBridge.RegisterBinarySensor(device, "is_active_source")
		}

		value := "off"
		if bridge.activeSource != nil && device.Id == bridge.activeSource.Id {
			value = "on"
		}

		bridge.mqtt.Publish(bridge.mqtt.BuildTopic(device, "is_active_source"), 0, true, value)
	}

	if bridge.haBridge != nil {
		bridge.haBridge.RegisterSensor("active_source", "active_source", bridge.mqtt.BuildBridgeTopic("active_source/physical_address"))
	}

	bridge.stateMutex.Lock()
	last := bridge.lastPhysicalAddress
	bridge.stateMutex.Unlock()

	if last != "" {
		bridge.mqtt.Publish(bridge.mqtt.BuildBridgeTopic("active_source/physical_address"), 0, true, last)
	}
}
