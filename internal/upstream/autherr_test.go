package upstream

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorDisplaysHTMLPageOnOneLine(t *testing.T) {
	page := "<html>\r\n<head><title>401 Authorization Required</title></head>\r\n<body>\r\n" +
		"<center><h1>401 Authorization Required</h1></center>\r\n<hr><center>openresty</center>\r\n"
	e := &Error{Kind: Classify(401, page), Status: 401, Msg: page}
	got := e.Error()
	if got != `upstream client (http 401): HTML page "401 Authorization Required"` {
		t.Errorf("Error() = %q", got)
	}
	if e.Msg != page {
		t.Error("Msg must keep the raw body (classification and passthrough use it)")
	}
	// JSON 原文：只折叠换行。
	j := &Error{Kind: ErrClient, Status: 400, Msg: "{\"code\":1,\n \"msg\":\"x\"}"}
	if s := j.Error(); strings.Contains(s, "\n") || !strings.Contains(s, `{"code":1, "msg":"x"}`) {
		t.Errorf("json Error() = %q", s)
	}
}

func TestIsAuthRejected(t *testing.T) {
	html401 := &Error{Kind: ErrClient, Status: 401, Msg: "<html><title>401 Authorization Required</title></html>"}
	if !IsAuthRejected(html401) || !IsAuthRejected(fmt.Errorf("wrapped: %w", html401)) {
		t.Error("an HTML 401 must count as an auth rejection")
	}
	dead := &Error{Kind: ErrSessionDead, Status: 401, Msg: `{"code":12153}`}
	if IsAuthRejected(dead) || !IsSessionDead(dead) {
		t.Error("12153 is session-dead, not a plain auth rejection")
	}
	for _, e := range []error{nil, errors.New("dial tcp: refused"), &Error{Kind: ErrWafBlock, Status: 403}, &Error{Kind: ErrServer, Status: 502}} {
		if IsAuthRejected(e) {
			t.Errorf("%v must not count as an auth rejection", e)
		}
	}
}
