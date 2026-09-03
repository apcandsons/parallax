package ui

import (
	"bytes"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyBytes(t *testing.T) {
	cases := []struct {
		msg  tea.KeyMsg
		want []byte
	}{
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")}, []byte("b")},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pasted text"), Paste: true}, []byte("pasted text")},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("é")}, []byte("é")},
		{tea.KeyMsg{Type: tea.KeySpace}, []byte(" ")},
		{tea.KeyMsg{Type: tea.KeyEnter}, []byte("\n")},
		{tea.KeyMsg{Type: tea.KeyTab}, []byte("\t")},
		{tea.KeyMsg{Type: tea.KeyBackspace}, []byte{0x7f}},
		{tea.KeyMsg{Type: tea.KeyEsc}, []byte{0x1b}},
		{tea.KeyMsg{Type: tea.KeyCtrlC}, []byte{3}},
		{tea.KeyMsg{Type: tea.KeyCtrlD}, []byte{4}},
		{tea.KeyMsg{Type: tea.KeyCtrlA}, []byte{1}},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x"), Alt: true}, []byte{0x1b, 'x'}},
		{tea.KeyMsg{Type: tea.KeyUp}, nil},
		{tea.KeyMsg{Type: tea.KeyF1}, nil},
	}
	for _, c := range cases {
		if got := keyBytes(c.msg); !bytes.Equal(got, c.want) {
			t.Errorf("keyBytes(%s) = %q, want %q", c.msg.String(), got, c.want)
		}
	}
}
