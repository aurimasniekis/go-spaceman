package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeAttachmentName(t *testing.T) {
	long := strings.Repeat("a", 300) + ".png"
	for in, want := range map[string]string{
		"shot.png":                "shot.png",
		"../x":                    "x",
		"../../etc/passwd":        "passwd",
		"/etc/passwd":             "passwd",
		`..\..\windows\evil.exe`:  "evil.exe",
		"..":                      "attachment",
		".":                       "attachment",
		"":                        "attachment",
		"/":                       "attachment",
		".bashrc":                 "bashrc",
		"...":                     "attachment",
		"Ekrano nuotrauka ąč.png": "Ekrano_nuotrauka___.png",
		"a\x00b\nc.txt":           "a_b_c.txt",
		"x/y*?[z].md":             "y___z_.md",
		long:                      strings.Repeat("a", maxSavedName-4) + ".png",
	} {
		got := safeAttachmentName(in)
		if got != want {
			t.Errorf("safeAttachmentName(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, `/\`) || strings.HasPrefix(got, ".") || len(got) > maxSavedName {
			t.Errorf("safeAttachmentName(%q) = %q is not a plain name", in, got)
		}
	}
}

// A run's file tools can write its own instance folder: a link planted as the attachments folder is refused, never
// followed.
func TestInstanceAttachmentsRefusesLinks(t *testing.T) {
	inst := t.TempDir()
	outside := t.TempDir()
	dir, err := instanceAttachments(inst, true)
	if err != nil || dir != filepath.Join(inst, attachmentsDir) {
		t.Fatalf("create: %q %v", dir, err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := instanceAttachments(inst, true); err == nil {
		t.Fatal("a linked attachments folder was accepted")
	}
	if _, err := instanceAttachments(filepath.Join(inst, "missing"), true); err == nil {
		t.Fatal("a missing instance folder was accepted")
	}
	link := filepath.Join(t.TempDir(), "inst")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := instanceAttachments(link, true); err == nil {
		t.Fatal("a linked instance folder was accepted")
	}
}
