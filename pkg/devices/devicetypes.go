package devices

// Device-type constants for camera families with special stream handling.
// Values mirror the eufy cloud API device_type field.
const (
	// Outdoor pan-tilt family (dual-lens: pan-tilt main lens + fixed wide lens).
	DeviceTypeOutdoorPTCamera    = 48  // SoloCam S340
	DeviceTypeSoloCameraE30      = 88  // SoloCam E30
	DeviceTypeCameraS4           = 89  // Camera S4 (T8172)
	DeviceTypeSolocamE42         = 98  // SoloCam E42 (T8173)
	DeviceTypeCamera4GS330       = 111 // 4G S330 (T86P2)

	// Indoor pan-tilt family.
	DeviceTypeIndoorPTCameraS350 = 104   // Indoor Cam S350 (dual-lens)
	DeviceTypeIndoorPTCameraE30  = 105   // Indoor Cam E30
	DeviceTypeIndoorPTCameraC220 = 10008 // Indoor Cam C220 (T8W11C)
	DeviceTypeIndoorPTCameraC210 = 10009 // Indoor Cam C210 (T8419)
	DeviceTypeIndoorPTCameraC220V2 = 10010
	DeviceTypeIndoorPTCameraC220V3 = 10011

	// Dual-lens battery doorbells.
	DeviceTypeBatteryDoorbellPlusE340 = 94 // Battery Doorbell Plus E340 (T8023)
	DeviceTypeBatteryDoorbellC30      = 95 // Battery Doorbell C30
)

// IsOutdoorPanTiltCamera reports whether the device is an outdoor pan-tilt
// camera. These stream H.265 by default.
func IsOutdoorPanTiltCamera(deviceType int) bool {
	switch deviceType {
	case DeviceTypeOutdoorPTCamera,
		DeviceTypeSoloCameraE30,
		DeviceTypeCameraS4,
		DeviceTypeSolocamE42,
		DeviceTypeCamera4GS330:
		return true
	}
	return false
}

// IsIndoorPanTiltCamera reports whether the device is an indoor pan-tilt
// camera (S350/E30/C210/C220 family).
func IsIndoorPanTiltCamera(deviceType int) bool {
	switch deviceType {
	case DeviceTypeIndoorPTCameraS350,
		DeviceTypeIndoorPTCameraE30,
		DeviceTypeIndoorPTCameraC210,
		DeviceTypeIndoorPTCameraC220,
		DeviceTypeIndoorPTCameraC220V2,
		DeviceTypeIndoorPTCameraC220V3:
		return true
	}
	return false
}

// IsBatteryDoorbellDual reports whether the device is a dual-lens battery
// doorbell (E340/C30 family).
func IsBatteryDoorbellDual(deviceType int) bool {
	return deviceType == DeviceTypeBatteryDoorbellPlusE340 ||
		deviceType == DeviceTypeBatteryDoorbellC30
}

// LensesForDeviceType returns how many simultaneous P2P video channels the
// device exposes for live viewing.
//
// IMPORTANT (verified live against a real account): multi-lens cameras like
// the Battery Doorbell Plus E340 deliver BOTH lenses composited into a single
// portrait stream on device_channel (1600x2200 = main lens + package lens
// stacked). Requesting device_channel+1 on a HomeBase-managed device grabs
// the NEXT DEVICE's slot on that homebase (e.g. the neighboring camera's
// feed) — it is NOT a second lens. Never add +1 channels for HomeBase-managed
// devices. Standalone (non-HomeBase) dual-lens installs may expose channel+1;
// only enable that if observed.
func LensesForDeviceType(deviceType int) int {
	return 1
}
