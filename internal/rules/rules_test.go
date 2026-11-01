package rules

import "testing"

func TestTakesMarkdownTextAndConvertedFormatsOnly(t *testing.T) {
	for _, p := range []string{"a.md", "Tax/2021 Return.PDF", "notes.txt", "sheet.xlsx", "page.htm"} {
		if !Takes(p) {
			t.Errorf("Takes(%q) = false", p)
		}
	}
	for _, p := range []string{"photo.jpg", "slides.pptx", "old.doc", "README", ".md"} {
		if Takes(p) {
			t.Errorf("Takes(%q) = true", p)
		}
	}
}

func TestClutterAndHidden(t *testing.T) {
	for _, n := range []string{".DS_Store", "Thumbs.db", "desktop.ini", "~$Report.docx", "._a.md"} {
		if !Clutter(n) {
			t.Errorf("Clutter(%q) = false", n)
		}
	}
	if Clutter("Report.docx") || !Hidden(".obsidian") || Hidden("notes") {
		t.Error("clutter or hidden misjudged")
	}
}

func TestSafePathRefusesWhatTheArchiveRefuses(t *testing.T) {
	if !SafePath("Letters/2021/thanks.md") {
		t.Error("a plain relative path was refused")
	}
	for _, p := range []string{"", "/abs.md", "../up.md", "a//b.md", "a/.hidden/b.md", "back\\slash.md", "tab\t.md"} {
		if SafePath(p) {
			t.Errorf("SafePath(%q) = true", p)
		}
	}
}

func TestRootForNamesTheArchiveFolder(t *testing.T) {
	cases := map[string]string{
		"Documents":         "archivist-sync-documents",
		"Tax Papers (2021)": "archivist-sync-tax-papers-2021",
		"Café":              "archivist-sync-cafe",
		"!!!":               "archivist-sync-folder",
	}
	for in, want := range cases {
		if got := RootFor(in); got != want || !ValidRoot(got) {
			t.Errorf("RootFor(%q) = %q, want %q", in, got, want)
		}
	}
	long := RootFor("a very long folder name that goes on and on and on and on and on")
	if len(long) > 64 || !ValidRoot(long) {
		t.Errorf("long root %q isn't valid", long)
	}
}

func TestDeletionGuard(t *testing.T) {
	if !NeedsDeletionConfirmation(21, 10) || NeedsDeletionConfirmation(20, 0) || NeedsDeletionConfirmation(30, 200) {
		t.Error("the guard's threshold is wrong")
	}
}
