package logbuf

import "testing"

func texts(lines []Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

func TestMergeOrder(t *testing.T) {
	s := NewStore(3, 10)
	s.Append(0, Stdout, "a1")
	s.Append(2, Stdout, "c1")
	s.Append(1, Stderr, "b1")
	s.Append(0, Stdout, "a2")

	got := texts(s.All())
	want := []string{"a1", "c1", "b1", "a2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("All() = %v, want %v (arrival order)", got, want)
		}
	}
	if one := texts(s.Proc(0)); len(one) != 2 || one[0] != "a1" || one[1] != "a2" {
		t.Fatalf("Proc(0) = %v, want [a1 a2]", one)
	}
}

func TestRingEviction(t *testing.T) {
	s := NewStore(1, 3)
	for _, txt := range []string{"1", "2", "3", "4", "5"} {
		s.Append(0, Stdout, txt)
	}
	got := texts(s.Proc(0))
	want := []string{"3", "4", "5"}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Proc(0) = %v, want %v (oldest evicted)", got, want)
		}
	}
	// All() over an evicted buffer stays consistent.
	if all := texts(s.All()); len(all) != 3 || all[0] != "3" {
		t.Fatalf("All() = %v, want [3 4 5]", all)
	}
}

func TestNotifyCoalesces(t *testing.T) {
	s := NewStore(1, 10)
	for i := 0; i < 100; i++ {
		s.Append(0, Stdout, "x")
	}
	select {
	case <-s.Notify():
	default:
		t.Fatal("expected a pending notification")
	}
	select {
	case <-s.Notify():
		t.Fatal("burst should coalesce into one notification")
	default:
	}
}
