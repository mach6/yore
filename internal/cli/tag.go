package cli

import (
	"fmt"
	"os"
	"strings"

	"yore/internal/daemon"
	"yore/internal/proto"
	"yore/internal/rec"
)

// runTagList prints every known user tag with how many commands carry it. scope
// bounds the count: "local" (the default) this host, "all" every host.
func runTagList(scope string) int {
	switch scope {
	case "", "local":
		scope = proto.ScopeLocal
	case "all":
		scope = proto.ScopeAll
	default:
		fmt.Fprintf(os.Stderr, "yore: unknown scope %q (want local or all)\n", scope)
		return 2
	}
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore: daemon unavailable:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	info, err := c.Tags(scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore:", err)
		return 1
	}
	if len(info.Tags) == 0 {
		fmt.Println("no tags yet — add one with `yore tag add <name>`")
		return 0
	}
	for _, t := range info.Tags {
		line := fmt.Sprintf("%-20s %4d", t.Name, t.Count)
		if t.Desc != "" {
			line += "  " + t.Desc
		}
		fmt.Println(line)
	}
	return 0
}

// submitTag builds and delivers one TypeTag record to the daemon.
func submitTag(r rec.Record) int {
	r.ID = rec.NewID()
	r.Type = rec.TypeTag
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore: daemon unavailable:", err)
		return 1
	}
	defer func() { _ = c.Close() }()
	if err := c.SubmitRecord(r); err != nil {
		fmt.Fprintln(os.Stderr, "yore:", err)
		return 1
	}
	return 0
}

// normTagName lowercases and trims a tag name (names are the identity).
func normTagName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// runTagCreate registers a tag name and optional description (no association).
func runTagCreate(name, desc string) int {
	name = normTagName(name)
	if name == "" {
		fmt.Fprintln(os.Stderr, "yore: tag name is required")
		return 1
	}
	if rc := submitTag(rec.Record{TagName: name, TagDesc: desc}); rc != 0 {
		return rc
	}
	fmt.Printf("created tag %q\n", name)
	return 0
}

// runTagAssociate adds a tag to a target: a specific command (--command), a
// specific session (--session), or the current shell session by default.
func runTagAssociate(name, command, session string, remove bool) int {
	name = normTagName(name)
	if name == "" {
		fmt.Fprintln(os.Stderr, "yore: tag name is required")
		return 1
	}
	r := rec.Record{TagName: name}
	if remove {
		r.TagOp = rec.TagOpRemove
	}
	switch {
	case command != "":
		r.TargetID = command
	case session != "":
		r.Session = session
	default:
		r.Session = os.Getenv("YORE_SESSION")
	}
	if r.TargetID == "" && r.Session == "" {
		fmt.Fprintln(os.Stderr, "yore: no target — pass --command <id>, --session <id>, or run inside a shell session")
		return 1
	}
	if rc := submitTag(r); rc != 0 {
		return rc
	}
	verb := "tagged"
	if remove {
		verb = "untagged"
	}
	target := "session " + r.Session
	if r.TargetID != "" {
		target = "command " + r.TargetID
	}
	fmt.Printf("%s %s with %q\n", verb, target, name)
	return 0
}
