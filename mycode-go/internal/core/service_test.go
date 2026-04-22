package core

import "testing"

func TestBuildUserMessageEscapesAttachmentNameLikePython(t *testing.T) {
	msg, err := buildUserMessage(ChatRequest{
		Input: []ChatInputBlock{
			{Type: "text", Text: "print(1)", Name: `report <"draft">.py`, IsAttachment: true},
		},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("unexpected message: %#v", msg)
	}
	if got := msg.Content[0].Text; got != "<file name=\"report &lt;&quot;draft&quot;&gt;.py\">\nprint(1)\n</file>" {
		t.Fatalf("unexpected attachment block: %q", got)
	}
}
