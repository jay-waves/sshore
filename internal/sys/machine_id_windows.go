package sys

import "golang.org/x/sys/windows/registry"

// MachineID reads the OS identifier and returns empty when unavailable.
func MachineID() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, _ := key.GetStringValue("MachineGuid")
	return value
}
