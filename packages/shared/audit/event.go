package audit

// SchemaVersion is the current audit event format version.
// Incremented when the event schema changes in a way that requires
// Hub to dispatch to a version-specific parser.
// Agents include this in their upload payload so the server can handle
// events from agents that have not yet updated.
const SchemaVersion = 1
