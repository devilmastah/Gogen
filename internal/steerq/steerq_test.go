package steerq

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

func textOf(items []Item) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = it.Text
	}
	return strings.Join(parts, ",")
}

// TestAppendUserRefusedAtCap pins the user-kind policy: typed text is never
// silently dropped, so a full user queue REFUSES the new item (the host turns
// that into a visible error).
func TestAppendUserRefusedAtCap(t *testing.T) {
	q := New(2, 32)
	for _, text := range []string{"a", "b"} {
		if got, _ := q.Append(Item{ID: text, Text: text, Kind: User}); got != Accepted {
			t.Fatalf("Append(%q) = %v before the cap, want Accepted", text, got)
		}
	}
	got, dropped := q.Append(Item{ID: "c", Text: "c", Kind: User})
	if got != Refused || dropped != nil {
		t.Fatalf("Append past the user cap = (%v, %v), want Refused", got, dropped)
	}
	if q.Len() != 2 {
		t.Fatalf("queue length = %d, want 2", q.Len())
	}
}

// TestAppendNoticeEvictsOldestNotice pins the system-kind policy: at the cap
// the OLDEST NOTICE goes, and the item that arrived is queued.
func TestAppendNoticeEvictsOldestNotice(t *testing.T) {
	q := New(0, 2) // unbounded user cap, system cap 2
	q.Append(Item{ID: "n1", Text: "n1", Kind: Notice})
	q.Append(Item{ID: "n2", Text: "n2", Kind: Notice})

	got, dropped := q.Append(Item{ID: "n3", Text: "n3", Kind: Notice})
	if got != Accepted {
		t.Fatalf("notice Append at the system cap = %v; a notice must evict a notice instead", got)
	}
	if dropped == nil || dropped.ID != "n1" {
		t.Fatalf("dropped = %+v, want the oldest notice n1", dropped)
	}
	if got := textOf(q.Items()); got != "n2,n3" {
		t.Fatalf("queue = %q, want n2,n3", got)
	}
}

// TestAppendNoticeNeverEvictsReply is the rule that protects a pending answer:
// with every system slot holding a targeted delivery, an arriving notice is
// dropped rather than evicting a reply whose parent is waiting on it.
func TestAppendNoticeNeverEvictsReply(t *testing.T) {
	q := New(0, 2)
	q.Append(Item{ID: "r1", Text: "r1", Kind: Reply})
	q.Append(Item{ID: "r2", Text: "r2", Kind: Reply})

	got, dropped := q.Append(Item{ID: "n1", Text: "n1", Kind: Notice})
	if got != Dropped || dropped != nil {
		t.Fatalf("notice over a full reply queue = (%v, %v), want Dropped", got, dropped)
	}
	if got := textOf(q.Items()); got != "r1,r2" {
		t.Fatalf("queue = %q, want the replies intact", got)
	}
}

// TestUserItemsDoNotCompeteWithSystemItems pins the independent caps.
func TestUserItemsDoNotCompeteWithSystemItems(t *testing.T) {
	q := New(1, 1)
	if got, _ := q.Append(Item{ID: "u1", Text: "u1", Kind: User}); got != Accepted {
		t.Fatalf("user item = %v", got)
	}
	if got, _ := q.Append(Item{ID: "n1", Text: "n1", Kind: Notice}); got != Accepted {
		t.Fatalf("system item = %v, refused by the user cap", got)
	}
	if got, _ := q.Append(Item{ID: "u2", Text: "u2", Kind: User}); got != Refused {
		t.Fatalf("user item past its own cap = %v, want Refused", got)
	}
	if got, _ := q.Append(Item{ID: "n2", Text: "n2", Kind: Notice}); got != Accepted {
		t.Fatalf("notice past the user cap = %v, want Accepted", got)
	}
}

// TestBatch pins the batching rule the two hosts used to duplicate: a
// contiguous head run of notices is one turn, a run of user messages is one
// turn only with join_queued, and anything else is its own.
func TestBatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds []Kind
		join  bool
		want  int
	}{
		{"single notice", []Kind{Notice}, false, 1},
		{"notice run", []Kind{Notice, Notice, Notice}, false, 3},
		{"notice run then user", []Kind{Notice, Notice, User, Notice}, false, 2},
		{"notice run then reply", []Kind{Notice, Reply, Notice}, false, 1},
		{"user head", []Kind{User, Notice}, false, 1},
		{"user run without join", []Kind{User, User, User}, false, 1},
		{"user run with join", []Kind{User, User, User}, true, 3},
		{"joined user run then notice", []Kind{User, User, Notice}, true, 2},
		{"reply head with join", []Kind{Reply, Reply}, true, 1},
		{"reply head", []Kind{Reply, Notice}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := New(0, 0)
			for i, k := range tc.kinds {
				q.Append(Item{ID: string(rune('a' + i)), Text: string(rune('a' + i)), Kind: k})
			}
			if got := len(q.Batch(tc.join)); got != tc.want {
				t.Fatalf("Batch(%v) = %d items, want %d", tc.join, got, tc.want)
			}
			if q.Len() != len(tc.kinds) {
				t.Fatalf("Batch mutated the queue: len = %d", q.Len())
			}
		})
	}
}

// TestBatchImageCap pins that a joined user run never carries more than
// MaxImagesPerMessage images: it stops before the message that would go over,
// and that message heads the next run.
func TestBatchImageCap(t *testing.T) {
	img := llm.ImageInput{DataURL: "data:image/png;base64,AA"}
	images := func(n int) []llm.ImageInput {
		out := make([]llm.ImageInput, n)
		for i := range out {
			out[i] = img
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		counts []int
		want   int
	}{
		{"text only joins", []int{0, 0, 0}, 3},
		{"exactly the cap joins", []int{2, 2}, 2},
		{"over the cap stops before it", []int{2, 2, 1}, 2},
		{"a full head stays alone", []int{MaxImagesPerMessage, 1}, 1},
		{"a full head still takes text", []int{MaxImagesPerMessage, 0, 1}, 2},
		{"many small ones stop at the cap", []int{1, 1, 1, 1, 1, 1}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := New(0, 0)
			for i, n := range tc.counts {
				q.Append(Item{ID: string(rune('a' + i)), Text: "m", Kind: User, Images: images(n)})
			}
			batch := q.Batch(true)
			if len(batch) != tc.want {
				t.Fatalf("Batch(true) = %d items, want %d", len(batch), tc.want)
			}
			if _, joined := Join(batch); len(joined) > MaxImagesPerMessage {
				t.Fatalf("joined run carries %d images, over the cap %d", len(joined), MaxImagesPerMessage)
			}
		})
	}
}

// TestJoin pins the one prompt a batch runs as: texts in queue order with a
// blank line between them, and every item's images in that same order.
func TestJoin(t *testing.T) {
	a := llm.ImageInput{DataURL: "data:image/png;base64,AA"}
	b := llm.ImageInput{DataURL: "data:image/png;base64,BB"}
	for _, tc := range []struct {
		name       string
		batch      []Item
		wantText   string
		wantImages []llm.ImageInput
	}{
		{"single item", []Item{{Text: "one", Images: []llm.ImageInput{a}}}, "one", []llm.ImageInput{a}},
		{"run", []Item{{Text: "one", Images: []llm.ImageInput{a}}, {Text: "two"}, {Text: "three", Images: []llm.ImageInput{b}}},
			"one\n\ntwo\n\nthree", []llm.ImageInput{a, b}},
		{"run without images", []Item{{Text: "one"}, {Text: "two"}}, "one\n\ntwo", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, images := Join(tc.batch)
			if text != tc.wantText {
				t.Fatalf("text = %q, want %q", text, tc.wantText)
			}
			if got := JoinText(tc.batch); got != text {
				t.Fatalf("JoinText = %q, want Join's text %q", got, text)
			}
			if len(images) != len(tc.wantImages) {
				t.Fatalf("images = %d, want %d", len(images), len(tc.wantImages))
			}
			for i := range images {
				if images[i].DataURL != tc.wantImages[i].DataURL {
					t.Fatalf("image %d = %q, want %q", i, images[i].DataURL, tc.wantImages[i].DataURL)
				}
			}
		})
	}
}

// TestSendNowReady pins when Send now may stop the running turn: only while
// a user message it would deliver is still queued.
func TestSendNowReady(t *testing.T) {
	q := New(0, 0)
	q.Append(Item{ID: "n1", Kind: Notice})
	for _, tc := range []struct {
		name string
		user bool
		id   string
		want bool
	}{
		{"only notices queued", false, "", false},
		{"notice id is not a user item", false, "n1", false},
		{"any user item", true, "", true},
		{"that user item", true, "u1", true},
		{"a user item that left", true, "u-gone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.user && !q.Has("u1", User) {
				q.Append(Item{ID: "u1", Kind: User})
			}
			if got := q.SendNowReady(tc.id); got != tc.want {
				t.Fatalf("SendNowReady(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// TestCheckSendNow pins the Send now rule and its order: idle first, then no
// model turn (a compaction), then a send_message reply, then the queue.
func TestCheckSendNow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		busy     bool
		turn     uint64
		replying bool
		queued   bool
		want     SendNowResult
	}{
		{"idle session", false, 0, false, true, SendNowIdle},
		{"idle even with a stale turn", false, 3, false, true, SendNowIdle},
		{"compaction has no model turn", true, 0, false, true, SendNowNoModelCall},
		{"reply turn is left alone", true, 3, true, true, SendNowReply},
		{"reply wins over an empty queue", true, 3, true, false, SendNowReply},
		{"nothing queued", true, 3, false, false, SendNowNotQueued},
		{"queued message stops the turn", true, 3, false, true, SendNowRecorded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckSendNow(tc.busy, tc.turn, tc.replying, tc.queued)
			if got != tc.want {
				t.Fatalf("CheckSendNow = %v, want %v", got, tc.want)
			}
			if got.Message() == "" {
				t.Fatalf("%v has no message", got)
			}
		})
	}
}

// TestTakeZeroesPoppedSlots is the M3 contract, now enforced in one place: a
// popped item must not stay reachable through the queue's backing array, which
// matters because a user item can carry multi-MB data-URL images.
func TestTakeZeroesPoppedSlots(t *testing.T) {
	q := New(0, 0)
	q.Append(Item{ID: "a", Kind: User, Images: []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA"}}})
	q.Append(Item{ID: "b", Kind: User})

	taken := q.Take(1)
	if len(taken) != 1 || taken[0].ID != "a" {
		t.Fatalf("Take(1) = %+v", taken)
	}
	// The zeroed slot is before the queue's current start, so inspect the
	// backing array through the full slice expression.
	for _, it := range q.items[:cap(q.items)] {
		if it.Images != nil {
			t.Fatal("popped item's images still reachable through the backing array")
		}
	}
}

// TestDropKindKeepsOtherKinds pins the interrupt semantic: the user's queued
// text goes, machine bookkeeping stays.
func TestDropKindKeepsOtherKinds(t *testing.T) {
	q := New(0, 0)
	for _, it := range []Item{
		{ID: "n1", Kind: Notice}, {ID: "u1", Kind: User},
		{ID: "r1", Kind: Reply}, {ID: "u2", Kind: User},
	} {
		q.Append(it)
	}
	dropped := q.DropKind(User)
	if len(dropped) != 2 {
		t.Fatalf("DropKind(User) removed %d, want 2", len(dropped))
	}
	ids := make([]string, 0, q.Len())
	for _, it := range q.Items() {
		ids = append(ids, it.ID)
	}
	if strings.Join(ids, ",") != "n1,r1" {
		t.Fatalf("remaining = %v, want n1,r1", ids)
	}
}

// TestRemoveAndHas covers the client-id correlation path.
func TestRemoveAndHas(t *testing.T) {
	q := New(0, 0)
	q.Append(Item{ID: "u1", Kind: User})
	q.Append(Item{ID: "n1", Kind: Notice})

	if !q.Has("u1", User) {
		t.Fatal("Has(u1, User) = false")
	}
	if q.Has("u1", Notice) {
		t.Fatal("Has must match the kind too")
	}
	if _, ok := q.Remove("n1", User); ok {
		t.Fatal("Remove must match the kind (a notice is not removable as a user item)")
	}
	if it, ok := q.Remove("n1", Notice); !ok || it.ID != "n1" {
		t.Fatalf("Remove(n1, Notice) = (%+v, %v)", it, ok)
	}
	if q.Len() != 1 {
		t.Fatalf("len = %d, want 1", q.Len())
	}
}

// TestNilQueueIsInert pins the zero-value safety the hosts rely on (a
// runtime/session whose queue was never created must not panic on a read).
func TestNilQueueIsInert(t *testing.T) {
	var q *Queue
	if q.Len() != 0 || q.Count(User) != 0 || q.Items() != nil || q.Batch(true) != nil || q.SendNowReady("") {
		t.Fatal("nil queue must read as empty")
	}
	if _, ok := q.Peek(); ok {
		t.Fatal("nil queue peeked an item")
	}
	if got, _ := q.Append(Item{Kind: Notice}); got != Refused {
		t.Fatal("nil queue accepted an append") // documents the requirement: hosts must New()
	}
}
