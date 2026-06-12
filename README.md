# Cec2Mqt
cec2mqtt enables you to read the status and control your CEC enabled devices using MQTT.
Currently its supported features are:
* Reading the power status (on/off) of devices
* Powering on and off devices
* Reading which device is active
* Publishing the active source's CEC physical address (e.g. ``2.0.0.0``), so the active HDMI input can be tracked even for ports without a CEC device
* Home Assistant integration for auto discovery

# Requirements
cec2mqtt currently works on the following hardware:
* Generic x64 (Intel / AMD) computers, ARMv7 (32 bit), and ARMv8 (64) bit devices using the Pulse-Eight HDMI-CEC adapter
* Any device running Linux with kernel support for CEC (like a Raspberry Pi)
Testing has been done both on a generic x64 computer using the Pulse-Eight HDMI-CEC adapter and on a Raspberry Pi.

## Installation
The easiest way to run cec2mqtt is by using the Docker images published to this fork's GitHub Container Registry. The latest
development build is available as ``ghcr.io/paltdariusz/cec2mqtt:edge``, and tagged releases such as ``ghcr.io/paltdariusz/cec2mqtt:0.0.4``
are published from version (``v*``) tags. For reproducible deployments it is recommended to pin a specific image by its digest
(``ghcr.io/paltdariusz/cec2mqtt@sha256:...``). All images are multi-arch and work on all supported platforms (amd64, armv7, arm64).

Running cec2mqtt can be done using:
```console
docker run -v /path/to/data/directory:/data/cec2mqtt --device=/dev/cec0 ghcr.io/paltdariusz/cec2mqtt:0.0.4
```
``/dev/cec0`` can be replaced with another CEC device if the system exposes more. Or use ``/dev/ttyACM0`` or equivalent if your kernel doesn't expose
any CEC devices and you're using the Pulse-Eight HDMI-CEC adapter.

When using docker-compose the following ``docker-compose.yaml`` can be used as a starting point:
```yaml
version: '3'

services:
  cec2mqtt:
    container_name: cec2mqtt
    image: ghcr.io/paltdariusz/cec2mqtt:0.0.4
    volumes:
      - ./data:/data/cec2mqtt
    devices:
      - /dev/cec0
    restart: unless-stopped
```

## Configuration
Configuring cec2mqtt is done by a YAML file which must be created before the first start. The file must be
placed in the data directory and be called ``config.yaml``. Required options to configure are the MQTT host and base topic.
A minimal configuration file looks like this:
```yaml
mqtt:
  host: 1.2.3.4:1883
  base_topic: cec2mqtt
```

To enable the Home Assistant integration the following configuration must be added:
```yaml
home_assistant:
  enable: true
```
Enabling this integration is the recommended way to use cec2mqtt in combination with Home Assistant as it removes the requirement to manually
configure the entities in Home Assistant.

Optionally an MQTT state topic with birth and will message can be configured (also required when using Home Assistant integration).
```yaml
mqtt:
  state_topic: cec2mqtt/state
  birth_message: online
  will_message: offline
```

A complete example of the configuration is the following:
```yaml
mqtt:
    host: 1.2.3.4:1883
    username: User
    password: P@ssw0rd
    state_topic: cec2mqtt/state
    birth_message: online
    will_message: offline
    base_topic: cec2mqtt
home_assistant:
    enable: true
    discovery_prefix: homeassistant
```

### Active source physical address
In addition to the per-device ``is_active_source`` topic, cec2mqtt publishes the CEC physical address of the
currently active source to the retained, bridge-level topic ``<base_topic>/active_source/physical_address`` (for the default
base topic: ``cec2mqtt/active_source/physical_address``). The value is a string like ``2.0.0.0`` where the first octet is the
HDMI input the TV is showing; it is set to ``0.0.0.0`` when the TV returns to its own tuner or goes to standby.

The topic is fed by three mechanisms (as of 0.0.5):
* An ``ACTIVE_SOURCE`` claim is published immediately — **unless** a routed target to a *different* address is pending, in which
  case the claim is a stale answer to an outdated routing request and is ignored (e.g. a resting PS5 woken by routing merely
  passing through its port claims it 5–11 s later, after the TV has already moved on). A claim matching the pending target
  confirms it and publishes early.
* A ``ROUTING_CHANGE`` / ``SET_STREAM_PATH`` target is published only after a 12 second delay, and only if no device claims that
  address in the meantime ("delayed trust"). This tracks inputs that have no CEC device behind them (a PC, an empty port) without
  trusting phantom routing sweeps, and it never publishes ``0.0.0.0`` — the TV announces its own tuner explicitly (a routing
  target of ``0.0.0.0`` still cancels any pending routed publish, so a quick port→tuner hop lands on the tuner announce).
* When the TV reports powering on, cec2mqtt broadcasts ``<Request Active Source>`` (at 8/16/24 s, stopping once anything claims),
  because some TVs (e.g. Panasonic Viera) announce nothing when they boot to their own tuner.

When the Home Assistant integration is enabled this is auto discovered as the ``sensor.cec2mqtt_active_source`` sensor.

### Device configuration
Devices which have been found in the CEC network can be configured as well. For this you **must** first stop cec2mqtt. When Cec2Mqtt is stopped you
can open the devices.yaml file in the data directory. Here you can change the ``mqtt_topic`` which is used in MQTT.

Optionally ``ignore`` can be set to ``true`` to completely ignore a device after which no cec2mqtt doesn't support this device anymore and
you can't read the state nor control the device.

Note that under normal operations you must never change any of the other values like, ``id``, ``physical_address``, ``vendor_id`` and ``osd`` 
as these are used by cec2mqtt to remember and look up the device.
