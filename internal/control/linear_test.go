package control

import (
	"context"
	"testing"
)

func TestCanonicalToLinearMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"*bold*", "**bold**"},
		{"_italic_", "*italic*"},
		{"~strike~", "~~strike~~"},
		{"`code`", "`code`"},
		{"plain text", "plain text"},
		{"snake_case stays", "snake_case stays"}, // mid-word _ is not italic
		{":tada: *PR opened* <https://github.com/o/r/pull/5|#5>", "🎉 **PR opened** [#5](https://github.com/o/r/pull/5)"},
	}
	for _, c := range cases {
		if got := canonicalToLinearMarkdown(c.in); got != c.want {
			t.Errorf("canonicalToLinearMarkdown(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

type fakeLinear struct {
	commentBody  string
	moved        []string
	uploadedName string
}

func (f *fakeLinear) CreateComment(_ context.Context, _, body string) (string, error) {
	f.commentBody = body
	return "cmt_1", nil
}
func (f *fakeLinear) UpdateComment(context.Context, string, string) error { return nil }
func (f *fakeLinear) MoveState(_ context.Context, _, stage string) error {
	f.moved = append(f.moved, stage)
	return nil
}
func (f *fakeLinear) UploadFile(_ context.Context, filename, _ string, _ []byte) (string, error) {
	f.uploadedName = filename
	return "https://uploads.linear.app/" + filename, nil
}

func TestLinearNotifier(t *testing.T) {
	f := &fakeLinear{}
	n := &linearNotifier{client: f}
	if n.Kind() != "linear" {
		t.Fatal("kind must be linear")
	}

	// Post returns Thread=issueId (so postRoot upserts conversation="linear:<id>"), and translates markup.
	res, err := n.Post(context.Background(), Conversation{Kind: "linear", Channel: "iss_9"}, ":tada: *done*")
	if err != nil || res.Thread != "iss_9" || res.Channel != "iss_9" || res.MessageID != "cmt_1" {
		t.Fatalf("Post = %+v, %v", res, err)
	}
	if f.commentBody != "🎉 **done**" {
		t.Errorf("comment body = %q", f.commentBody)
	}

	// Advance moves state; works whether the issue id is in Channel or Thread.
	_ = n.Advance(context.Background(), Conversation{Kind: "linear", Channel: "iss_9"}, "in_review")
	_ = n.Advance(context.Background(), Conversation{Kind: "linear", Thread: "iss_9"}, "done")
	if len(f.moved) != 2 || f.moved[0] != "in_review" || f.moved[1] != "done" {
		t.Errorf("moved = %v, want [in_review done]", f.moved)
	}

	// PostImage uploads the PNG and embeds it in a comment, caption (translated) above the image.
	id, err := n.PostImage(context.Background(), Conversation{Kind: "linear", Channel: "iss_9"}, ":tada: *shot*", []byte("PNG"), "shot.png")
	if err != nil || id != "cmt_1" {
		t.Fatalf("PostImage = (%q,%v)", id, err)
	}
	if f.uploadedName != "shot.png" {
		t.Errorf("uploaded filename = %q", f.uploadedName)
	}
	if want := "🎉 **shot**\n\n![shot.png](https://uploads.linear.app/shot.png)"; f.commentBody != want {
		t.Errorf("image comment body = %q, want %q", f.commentBody, want)
	}
}
