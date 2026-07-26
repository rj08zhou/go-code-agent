package tool

import "go-code-agent/internal/security"

// builtinDeps holds session-scoped services captured by builtin tool factories.
// Kept unexported so the public BuiltinTools signature stays stable.
type builtinDeps struct {
	taskSvc     TaskService
	todoSvc     TodoService
	memorySvc   MemoryService
	skillLoader SkillLoader
	bgSvc       BackgroundService
	bus         MessageBus
	subagentSvc SubagentService
	teamSvc     TeamService
	protocolSvc TeamProtocolService
	webSvc      WebService
	perms       *security.Permissions
}
