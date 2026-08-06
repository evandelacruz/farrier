// Package driver implements the CORE-003 exec-based driver protocol: the
// mechanism that lets a driver extension point — DNS, keystore, blob, and
// any future one — be satisfied by a standalone executable instead of
// in-tree Go code.
//
// Each extension point publishes its own Go interface for in-tree drivers
// (e.g. a future dns.Driver, keystore.Driver, blob.Adapter) plus its own
// method names and request/result shapes. This package supplies the one
// piece shared by all of them: Invoker, the seam a domain interface is
// built on, and Exec, the concrete Invoker that reaches a third-party
// executable by running it once per call, writing a Request as JSON to its
// stdin, and reading a Response as JSON from its stdout. A driver-type
// package wraps an Exec behind its own domain interface; third parties
// implement the protocol in any language, without linking Go at all.
package driver

import "encoding/json"

// Request is the JSON envelope an exec driver reads once, in full, from
// stdin before producing a Response and exiting. Method names the
// operation being called (e.g. "set", "resolve", "get") and is defined by
// whichever driver-type package uses this protocol, not by this package.
// Params carries that operation's arguments, encoded by the caller and
// decoded by the driver executable.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the JSON envelope an exec driver writes once, in full, to
// stdout before exiting. OK true means the call succeeded and Result holds
// its return value, if the operation has one; OK false means the call
// failed and Error names why.
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}
