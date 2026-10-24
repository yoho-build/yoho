package tui

import "github.com/yoho-build/yoho/internal/dash"

// DetailView pins a container's identity while snapshots and row order change.
type DetailView struct {
	Server, Container, ID string
	Offset                int
}

func (m *Model) detailContainer() (*dash.Server, *dash.Container) {
	if m.Details == nil || m.Snap == nil {
		return nil, nil
	}
	d := m.Details
	for i := range m.Snap.Servers {
		s := &m.Snap.Servers[i]
		if s.Name != d.Server {
			continue
		}
		matches := func(c *dash.Container) bool {
			return c.Name == d.Container && (d.ID == "" || c.ID == d.ID)
		}
		if s.Proxy != nil && matches(s.Proxy) {
			return s, s.Proxy
		}
		for j := range s.Containers {
			if matches(&s.Containers[j]) {
				return s, &s.Containers[j]
			}
		}
		return s, nil
	}
	return nil, nil
}

func (m *Model) updateDetails(k Key, height int) Action {
	page := max(1, height-6)
	switch k {
	case KeyEsc, "q", KeyLeft, "h":
		m.Details = nil
	case "r":
		return ActRefresh
	case KeyUp, "k":
		m.Details.Offset = max(0, m.Details.Offset-1)
	case KeyDown, "j":
		m.Details.Offset++
	case KeyPageUp, "b":
		m.Details.Offset = max(0, m.Details.Offset-page)
	case KeyPageDown, " ", "f":
		m.Details.Offset += page
	case "g":
		m.Details.Offset = 0
	case "G":
		m.Details.Offset = 1 << 30 // The renderer clamps to the last page.
	case KeyEnter, "l":
		s, c := m.detailContainer()
		if c != nil && s.Error == "" {
			m.Logs = &LogView{Server: s.Name, Container: c.Name, Service: c.Service, Loading: true}
			return ActLogs
		}
	}
	return ActNone
}
