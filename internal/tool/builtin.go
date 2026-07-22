package tool

import "go-code-agent/internal/security"

// BuiltinTools returns the built-in tool definitions in a stable order.
// Order is part of the prompt-prefix cache contract — do not reorder casually.
func BuiltinTools(
	taskSvc TaskService,
	todoSvc TodoService,
	memorySvc MemoryService,
	skillLoader SkillLoader,
	bgSvc BackgroundService,
	bus MessageBus,
	subagentSvc SubagentService,
	teamSvc TeamService,
	protocolSvc TeamProtocolService,
	webSvc WebService,
	perms *security.Permissions,
) []ToolDefinition {
	d := builtinDeps{
		taskSvc:     taskSvc,
		todoSvc:     todoSvc,
		memorySvc:   memorySvc,
		skillLoader: skillLoader,
		bgSvc:       bgSvc,
		bus:         bus,
		subagentSvc: subagentSvc,
		teamSvc:     teamSvc,
		protocolSvc: protocolSvc,
		webSvc:      webSvc,
		perms:       perms,
	}

	shell := shellTools(d)             // bash, background_run, check_background
	fsRead := filesystemReadTools(d)   // read_file, list_dir, search_file, search_content
	fsWrite := filesystemWriteTools(d) // write_file, edit_file, delete_file, insert_file

	defs := make([]ToolDefinition, 0, 39)
	// Preserve historical registration order for prompt-prefix caching.
	defs = append(defs, shell[0])      // bash
	defs = append(defs, fsRead[0])     // read_file
	defs = append(defs, fsWrite...)    // write/edit/delete/insert
	defs = append(defs, fsRead[1:]...) // list_dir/search_file/search_content
	defs = append(defs, taskTools(d)...)
	defs = append(defs, memoryTools(d)...)
	defs = append(defs, shell[1:]...) // background_run/check_background
	defs = append(defs, teamTools(d)...)
	defs = append(defs, protocolTools(d)...)
	defs = append(defs, metaTools(d)...)
	defs = append(defs, webTools(d)...)
	return defs
}
