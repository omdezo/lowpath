package compat

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// windowsSystem reads the inventory with calls that need no administrator
// rights: the service manager's list (SC_MANAGER_ENUMERATE_SERVICE is
// granted to every signed-in user), the registry's service and Uninstall
// keys (readable by Users), and the IP Helper adapter list.
type windowsSystem struct {
	procs ProcessLister
	route RouteFinder
}

// NewSystem returns this OS's System: running processes from procs, the
// internet route from route.
func NewSystem(procs ProcessLister, route RouteFinder) System {
	return windowsSystem{procs: procs, route: route}
}

func (w windowsSystem) Processes() ([]string, error) { return processNames(w.procs), nil }

func (w windowsSystem) DefaultRoute() (int, error) { return routeIndex(w.route) }

// Services lists every Win32 service and driver with its state. Only
// WinDivert's file path is read (from the registry): it is the one whose
// folder tells Lowpath's driver from another program's.
func (windowsSystem) Services() ([]Service, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, fmt.Errorf("open the service manager: %w", err)
	}
	defer windows.CloseServiceHandle(m)
	var (
		buf    = make([]byte, 64<<10)
		out    []Service
		resume uint32
	)
	for {
		var needed, n uint32
		err := windows.EnumServicesStatusEx(m, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32|windows.SERVICE_DRIVER,
			windows.SERVICE_STATE_ALL, &buf[0], uint32(len(buf)), &needed, &n, &resume, nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, fmt.Errorf("list services: %w", err)
		}
		if n > 0 {
			for _, e := range unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), n) {
				s := Service{
					Name:    windows.UTF16PtrToString(e.ServiceName),
					Display: windows.UTF16PtrToString(e.DisplayName),
					Driver:  e.ServiceStatusProcess.ServiceType&windows.SERVICE_DRIVER != 0,
					Running: e.ServiceStatusProcess.CurrentState == windows.SERVICE_RUNNING,
				}
				if strings.HasPrefix(strings.ToLower(s.Name), "windivert") {
					s.Path = serviceImagePath(s.Name)
				}
				out = append(out, s)
			}
		}
		if err == nil {
			return out, nil
		}
		// ERROR_MORE_DATA: resume continues after what was returned; grow
		// the buffer when not even one entry fit.
		if n == 0 {
			if needed <= uint32(len(buf)) || needed > 16<<20 {
				return out, fmt.Errorf("list services: reply of %d bytes", needed)
			}
			buf = make([]byte, needed)
		}
	}
}

// serviceImagePath reads a service's ImagePath, or "" when it can't.
func serviceImagePath(name string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+name, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("ImagePath")
	if err != nil {
		return ""
	}
	return v
}

// Programs lists the display names under the Uninstall keys (what Settings
// → Apps shows): for all users in both registry views, and for this user.
func (windowsSystem) Programs() ([]string, error) {
	const path = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
	roots := []struct {
		key    registry.Key
		access uint32
	}{
		{registry.LOCAL_MACHINE, registry.WOW64_64KEY},
		{registry.LOCAL_MACHINE, registry.WOW64_32KEY},
		{registry.CURRENT_USER, 0},
	}
	var out []string
	var errs []error
	seen := map[string]bool{}
	for _, r := range roots {
		k, err := registry.OpenKey(r.key, path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE|r.access)
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		subs, err := k.ReadSubKeyNames(-1)
		if err != nil {
			errs = append(errs, err)
		}
		for _, sub := range subs {
			if name := displayName(k, sub); name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		k.Close()
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func displayName(parent registry.Key, sub string) string {
	k, err := registry.OpenKey(parent, sub, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("DisplayName")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// Adapters lists every network interface with its driver description and
// IANA type.
func (windowsSystem) Adapters() ([]Adapter, error) {
	const flags = windows.GAA_FLAG_SKIP_UNICAST | windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER
	size := uint32(16 << 10)
	for range 4 {
		buf := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, first, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			continue // size now says how much is needed (adapters can appear in between)
		}
		if err != nil {
			return nil, fmt.Errorf("list network adapters: %w", err)
		}
		var out []Adapter
		for a := first; a != nil; a = a.Next {
			idx := a.IfIndex
			if idx == 0 {
				idx = a.Ipv6IfIndex
			}
			out = append(out, Adapter{
				Index:       int(idx),
				Name:        windows.UTF16PtrToString(a.FriendlyName),
				Description: windows.UTF16PtrToString(a.Description),
				Type:        a.IfType,
				Up:          a.OperStatus == windows.IfOperStatusUp,
			})
		}
		return out, nil
	}
	return nil, errors.New("list network adapters: the list kept growing")
}
