package main

import (
	"github.com/RobertMe/gocec"
	log "github.com/sirupsen/logrus"
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
}

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

		bridge.publishPhysicalAddress(gocec.PhysicalAddress{params[0], params[1]})
	}, gocec.OpcodeActiveSource)

	cec.RegisterMessageHandler(func(message gocec.Message) {
		if message.Source() == gocec.DeviceTV {
			bridge.publishPhysicalAddress(gocec.PhysicalAddress{0x00, 0x00})
		}
	}, gocec.OpcodeStandby, gocec.OpcodeInactiveSource)

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
		if pa := bridge.cec.connection.GetPhysicalAddress(address); pa != (gocec.PhysicalAddress{0xFF, 0xFF}) {
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
	if bridge.lastPhysicalAddress != "" {
		bridge.mqtt.Publish(bridge.mqtt.BuildBridgeTopic("active_source/physical_address"), 0, true, bridge.lastPhysicalAddress)
	}
}
