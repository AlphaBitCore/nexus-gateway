// Shared TypeScript shapes returned by both the agent statusapi
// (local IPC) and the Control Plane HTTP API. Admin-only fields are
// optional so the agent path can omit them without breaking consumers
// that render the shared shape.
//
// CP UI may keep its own richer types (e.g. `AgentDevice`) extending
// these for admin-only sections. The Wails Dashboard works directly
// against these.

export * from './device';
export * from './audit';
export * from './policy';
export * from './enrollment';
