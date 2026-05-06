package devices

type DeviceType int

const (
	DeviceTypeUnknown       DeviceType = -1
	DeviceTypeStation        DeviceType = 0
	DeviceTypeCamera2       DeviceType = 1
	DeviceTypeIndoorCamera  DeviceType = 2
	DeviceTypeOutdoorCamera DeviceType = 3
	DeviceTypeDoorbell      DeviceType = 4
	DeviceTypeFloodlight    DeviceType = 5
)

type PropertyName string

const (
	PropertyNameDeviceRTSPStream  PropertyName = "device_rtsp_stream"
)

type CommandName string

const (
	CommandNameDeviceStartLivestream CommandName = "DeviceStartLivestream"
)
