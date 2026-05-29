package sem

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func writeText(out io.Writer, result Result) {
	styles := newTextStyles(out)
	fmt.Fprintf(out, "%s %s\n\n", styles.title("Semantic changes"), styles.dim(result.Base+".."+result.Head))
	if len(result.Files) == 0 {
		fmt.Fprintln(out, styles.dim("No semantic entity changes detected."))
		return
	}

	for _, file := range result.Files {
		if file.OldPath != "" && file.OldPath != file.Path {
			fmt.Fprintf(out, "%s %s %s", styles.file(file.OldPath), styles.dim("->"), styles.file(file.Path))
		} else {
			fmt.Fprint(out, styles.file(file.Path))
		}
		if file.Language != "" {
			fmt.Fprintf(out, " %s", styles.dim("("+file.Language+")"))
		}
		fmt.Fprintln(out)
		for _, change := range file.Changes {
			fmt.Fprintf(out, "  %s\n", styles.describe(change))
		}
		fmt.Fprintln(out)
	}
}

func describe(change EntityChange) string {
	dependents := dependentSuffix(change)
	switch change.Type {
	case "added":
		return fmt.Sprintf("+ %s %s added", change.Kind, change.Name)
	case "removed":
		return fmt.Sprintf("- %s %s removed%s", change.Kind, change.Name, dependents)
	case "renamed":
		return fmt.Sprintf("~ %s %s renamed from %s%s", change.Kind, change.NewName, change.OldName, dependents)
	case "signature_changed":
		return fmt.Sprintf("~ %s %s signature changed%s", change.Kind, change.Name, dependents)
	case "body_changed":
		return fmt.Sprintf("~ %s %s body changed%s", change.Kind, change.Name, dependents)
	default:
		return fmt.Sprintf("~ %s %s changed%s", change.Kind, change.Name, dependents)
	}
}

func dependentSuffix(change EntityChange) string {
	if change.Type == "added" {
		return ""
	}
	if change.DependentsCount == 1 {
		return " (1 dependent)"
	}
	return fmt.Sprintf(" (%d dependents)", change.DependentsCount)
}

type textStyles struct {
	color bool
}

func newTextStyles(out io.Writer) textStyles {
	return textStyles{color: shouldUseColor(out)}
}

func shouldUseColor(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("ENTIRE_SEM_FORCE_COLOR") != "" || os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (s textStyles) title(value string) string {
	return s.render("1;38;2;251;146;60", value)
}

func (s textStyles) file(value string) string {
	return s.render("1", value)
}

func (s textStyles) dim(value string) string {
	return s.render("90", value)
}

func (s textStyles) added(value string) string {
	return s.render("32", value)
}

func (s textStyles) removed(value string) string {
	return s.render("31", value)
}

func (s textStyles) changed(value string) string {
	return s.render("33", value)
}

func (s textStyles) render(code, value string) string {
	if !s.color || value == "" {
		return value
	}
	return "\x1b[" + code + "m" + value + "\x1b[0m"
}

func (s textStyles) describe(change EntityChange) string {
	dependents := dependentSuffix(change)
	dependents = s.dim(dependents)
	switch change.Type {
	case "added":
		return fmt.Sprintf("%s %s %s %s", s.added("+"), change.Kind, s.file(change.Name), s.added("added"))
	case "removed":
		return fmt.Sprintf("%s %s %s %s%s", s.removed("-"), change.Kind, s.file(change.Name), s.removed("removed"), dependents)
	case "renamed":
		return fmt.Sprintf("%s %s %s %s %s%s", s.changed("~"), change.Kind, s.file(change.NewName), s.changed("renamed from"), s.file(change.OldName), dependents)
	case "signature_changed":
		return fmt.Sprintf("%s %s %s %s%s", s.changed("~"), change.Kind, s.file(change.Name), s.changed("signature changed"), dependents)
	case "body_changed":
		return fmt.Sprintf("%s %s %s %s%s", s.changed("~"), change.Kind, s.file(change.Name), s.changed("body changed"), dependents)
	default:
		return fmt.Sprintf("%s %s %s %s%s", s.changed("~"), change.Kind, s.file(change.Name), s.changed("changed"), dependents)
	}
}
