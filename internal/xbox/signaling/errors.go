// Adapted from minecraft/service/signaling in github.com/hashimthearab/gophertunnel
// (commit 9331d09, a fork of github.com/sandertv/gophertunnel), MIT License,
// Copyright (c) 2019 Sandertv. See NOTICE in this folder.

package signaling

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Error is a delivery failure the signaling service reports for a signal sent through a Conn.
// [Conn.Signal] returns it when the service could not deliver an offer.
type Error struct {
	// Code identifies the type of error, such as ErrorCodeDeliveryFailure.
	Code int `json:"Code"`
	// Message represents the Error in a string.
	Message string `json:"Message"`
}

// Error returns a string representing code and message of the Error. It implements an error.
func (err *Error) Error() string {
	b := &strings.Builder{}
	b.WriteString("service/signaling: code ")
	b.WriteString(strconv.Itoa(err.Code))
	if err.Message != "" {
		b.WriteByte(':')
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(err.Message))
	}
	return b.String()
}

const (
	// ErrorCodePlayerNotFound indicates that the recipient of a signal was not found. It is
	// currently unused, and ErrorCodeDeliveryFailure is instead used for this purpose.
	ErrorCodePlayerNotFound = iota + 1
	// ErrorCodeDeliveryFailure indicates that the signaling service couldn't deliver
	// the message sent by the Conn to its recipient. It is also returned when
	// the recipient is no longer valid.
	ErrorCodeDeliveryFailure
)

// ErrCredentialsCall identifies failures while calling the signaling service for TURN credentials.
var ErrCredentialsCall = errors.New("service/signaling: credentials call failed")

// CredentialsError wraps a failed credentials JSON-RPC call.
type CredentialsError struct {
	// Method is the JSON-RPC method used for the failed credentials request.
	Method string
	// Err is the underlying call failure.
	Err error
}

func (e *CredentialsError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return fmt.Sprintf("call %q", e.Method)
	}
	return fmt.Sprintf("call %q: %v", e.Method, e.Err)
}

// Unwrap returns the underlying credentials call failure.
func (e *CredentialsError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is reports whether target is [ErrCredentialsCall].
func (e *CredentialsError) Is(target error) bool {
	return target == ErrCredentialsCall
}
