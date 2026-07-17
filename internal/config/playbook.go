package config

import "embed"

//go:embed templates/dev-playbook.md templates/reviewer-playbook.md
var playbookTemplates embed.FS

// PlaybookTemplate returns the embedded playbook `init` writes to a role's
// configured playbook path. ok is false for a role with no shipped template.
func PlaybookTemplate(role string) (content []byte, ok bool) {
	b, err := playbookTemplates.ReadFile("templates/" + role + "-playbook.md")
	if err != nil {
		return nil, false
	}
	return b, true
}
