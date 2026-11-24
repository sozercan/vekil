package main

import "errors"

var errDialogCanceled = errors.New("dialog canceled")

type confirmationPrompt struct {
	Title        string
	Message      string
	ApproveLabel string
	DeclineLabel string
}
