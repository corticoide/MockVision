package plugin

// Permissions a plugin declares in its manifest and the user approves
// before it runs (D86). What a plugin may do without any: render
// templates, log, report requests, gaps and clients, and ask for the
// faults of its instance.
const (
	// PermNetListen hands the plugin the sockets of its ports.
	PermNetListen = "net.listen"
	// PermNetConnect lets it open connections, to deliver events; without
	// it the plugin cannot connect anywhere where the kernel can enforce it.
	PermNetConnect    = "net.connect"
	PermAccountsRead  = "accounts.read"
	PermStateRead     = "state.read"
	PermStateWrite    = "state.write"
	PermEventsEmit    = "events.emit"
	PermEventsDeliver = "events.deliver"
	PermMediaRead     = "media.read"
	PermSD            = "sd"
)

// Permissions lists every permission, in the order the panel shows them.
var Permissions = []string{PermNetListen, PermNetConnect, PermAccountsRead, PermStateRead, PermStateWrite, PermEventsEmit,
	PermEventsDeliver, PermMediaRead, PermSD}

// ValidPermission says whether p is a permission.
func ValidPermission(p string) bool {
	for _, q := range Permissions {
		if q == p {
			return true
		}
	}
	return false
}
