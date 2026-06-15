package assistant

// handler_turn_artifacts.go — the structured-artifact lifting for one chat
// turn: a successful write_file's output carries a stable file-id marker, and
// this half turns it into a `file` SSE event so the widget renders a download
// button without scraping model prose.

// liftFileArtifact inspects one finished tool call and surfaces a `file` event
// for a successful write_file.
func liftFileArtifact(name string, output []byte, isErr bool, pub func(event string, payload any)) {
	if isErr || name != "write_file" {
		return
	}
	if id, ok := fileIDFromToolOutput(string(output)); ok {
		pub("file", map[string]string{"id": id, "downloadPath": assistantFilesPath + id})
	}
}
