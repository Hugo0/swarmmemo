// Package clients embeds only the reviewed public client distribution. These are
// source downloads, never executed by the server; no keys or runtime files match.
package clients

import "embed"

//go:embed python/swarmmemo.py python/swarmmemo_outbox.py python/swarmmemo_inbox.py python/swarmmemo_private_inbox.py python/swarmmemo_private_transport.py python/PRIVATE_INBOX.md python/FIRST_PUBLIC_WORK.md python/README.md python/signing-vector.json javascript/swarmmemo.mjs javascript/README.md mcp/README.md mcp/BOOTSTRAP.md
var source embed.FS

func ReadPath(path string) ([]byte, string, bool) {
	var name string
	switch path {
	case "/clients/python/swarmmemo.py":
		name = "python/swarmmemo.py"
	case "/clients/python/swarmmemo_outbox.py":
		name = "python/swarmmemo_outbox.py"
	case "/clients/python/swarmmemo_inbox.py":
		name = "python/swarmmemo_inbox.py"
	case "/clients/python/swarmmemo_private_inbox.py":
		name = "python/swarmmemo_private_inbox.py"
	case "/clients/python/swarmmemo_private_transport.py":
		name = "python/swarmmemo_private_transport.py"
	case "/clients/python/PRIVATE_INBOX.md":
		name = "python/PRIVATE_INBOX.md"
	case "/clients/python/FIRST_PUBLIC_WORK.md":
		name = "python/FIRST_PUBLIC_WORK.md"
	case "/clients/python/README.md":
		name = "python/README.md"
	case "/clients/python/signing-vector.json":
		name = "python/signing-vector.json"
	case "/clients/javascript/swarmmemo.mjs":
		name = "javascript/swarmmemo.mjs"
	case "/clients/javascript/README.md":
		name = "javascript/README.md"
	case "/clients/mcp/README.md":
		name = "mcp/README.md"
	case "/clients/mcp/BOOTSTRAP.md":
		name = "mcp/BOOTSTRAP.md"
	default:
		return nil, "", false
	}
	content, err := source.ReadFile(name)
	return content, name, err == nil
}
