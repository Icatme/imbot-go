package receive

import (
	"errors"
	"testing"
)

func TestPolicyDefaultsAndDisabledKinds(t *testing.T) {
	for _, kind := range []Kind{Text, Image, Voice, Video, File, Interaction, ToolCall} {
		c := Content{Kinds: []Kind{Text, kind}, Attachments: []Attachment{{Kind: kind}}}
		if err := (Policy{}).Check(c); err != nil {
			t.Fatalf("default rejected %s: %v", kind, err)
		}
		p := Policy{Disabled: []Kind{kind}}
		if err := p.Check(c); !errors.Is(err, ErrRejected) {
			t.Fatalf("disabled %s: %v", kind, err)
		}
	}
	// Attachment kinds cannot be hidden by an incomplete text projection.
	if err := (Policy{Disabled: []Kind{Image}}).Check(Content{Attachments: []Attachment{{Kind: Image}}}); !errors.Is(err, ErrRejected) {
		t.Fatal(err)
	}
}

func TestFileAllowlists(t *testing.T) {
	p := Policy{FileExtensions: []string{" .PDF ", "docx"}, FileMIMETypes: []string{"application/pdf", "APPLICATION/MSWORD"}}
	for _, tc := range []struct {
		name, mime string
		reason     Reason
	}{
		{"report.PdF", "application/pdf; charset=binary", ""},
		{"report.docx", "application/msword", ""},
		{"report.pdf.exe", "application/pdf", FileExtension},
		{"report.pdf ", "application/pdf", FileExtension},
		{"report.pdf\x00", "application/pdf", FileExtension},
		{"report.pdf", "image/png", FileMIME},
		{"report.pdf", "", FileMIME},
		{"report.pdf", "application/pdf; bad", FileMIME},
		{"", "application/pdf", FileExtension},
		{"README", "application/pdf", FileExtension},
	} {
		t.Run(tc.name+"/"+tc.mime, func(t *testing.T) {
			err := p.Check(Content{Attachments: []Attachment{{Kind: File, Name: tc.name, MIME: tc.mime}}})
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var rejected *Rejection
			if !errors.As(err, &rejected) || rejected.Reason != tc.reason || rejected.Kind != File {
				t.Fatalf("rejection=%v, want %s", err, tc.reason)
			}
		})
	}
	// File allowlists do not accidentally disable images or voice messages.
	if err := p.Check(Content{Attachments: []Attachment{{Kind: Image}, {Kind: Voice}}}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyInvalidConfiguration(t *testing.T) {
	for _, p := range []Policy{
		{Disabled: []Kind{"typo"}},
		{FileExtensions: []string{""}}, {FileExtensions: []string{"."}},
		{FileExtensions: []string{"*.pdf"}}, {FileExtensions: []string{"tar.gz"}},
		{FileExtensions: []string{"pdf/exe"}},
		{FileMIMETypes: []string{""}}, {FileMIMETypes: []string{"pdf"}},
		{FileMIMETypes: []string{"application/*"}},
		{FileMIMETypes: []string{"application/pdf; charset=utf-8"}},
	} {
		if err := p.Check(Content{}); !errors.Is(err, ErrInvalidPolicy) || errors.Is(err, ErrRejected) {
			t.Fatalf("invalid policy=%+v err=%v", p, err)
		}
	}
}

func TestFileKindWithoutDescriptorNeedsAllowlistMetadata(t *testing.T) {
	c := Content{Kinds: []Kind{File}}
	if err := (Policy{}).Check(c); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Policy{{FileExtensions: []string{"pdf"}}, {FileMIMETypes: []string{"application/pdf"}}} {
		if err := p.Check(c); !errors.Is(err, ErrRejected) {
			t.Fatalf("unknown file allowed: %v", err)
		}
	}
}
