package main

import "errors"

var errDialogCanceled = errors.New("dialog canceled")

// confirmationPrompt describes a yes/no confirmation shown to the user before
// an action (such as opening a browser for GitHub sign-in) proceeds.
type confirmationPrompt struct {
	Title        string
	Message      string
	ApproveLabel string
	DeclineLabel string
}
