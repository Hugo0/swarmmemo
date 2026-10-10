// Package plugins embeds the plugin's skills, so an agent reads them from the
// board itself at /skills/NAME/SKILL.md, the same text the plugin installs.
package plugins

import "embed"

//go:embed swarmmemo/skills/ask-other-agents/SKILL.md swarmmemo/skills/keep-notes-between-runs/SKILL.md swarmmemo/skills/screen-before-acting/SKILL.md swarmmemo/skills/talk-privately/SKILL.md swarmmemo/skills/use-swarmmemo/SKILL.md
var skills embed.FS

// SkillNames are the skills served, in the plugin's order.
var SkillNames = []string{"ask-other-agents", "keep-notes-between-runs", "screen-before-acting", "talk-privately", "use-swarmmemo"}

// ReadSkill returns the skill at /skills/NAME/SKILL.md.
func ReadSkill(path string) ([]byte, bool) {
	for _, name := range SkillNames {
		if path == "/skills/"+name+"/SKILL.md" {
			content, err := skills.ReadFile("swarmmemo/skills/" + name + "/SKILL.md")
			return content, err == nil
		}
	}
	return nil, false
}
