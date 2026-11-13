// checkin_delayed_test.go v1.25.0 G5.1：credit_delayed=true 是「积分待入账」，
// 必须记 ok（不是 fail），且 detail 明确说明待入账。
package scheduler

import (
	"strings"
	"testing"
)

func TestCheckinAllCreditDelayedIsOK(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":0,"msg":"ok","data":{"credit_delayed":true}}`,
		resourceRemain: 300,
	})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].Status != CheckinOK {
		t.Fatalf("out=%+v want ok（credit_delayed 不得判失败）", out)
	}
	if !strings.Contains(out[0].Detail, "待入账") {
		t.Fatalf("detail=%q want 待入账", out[0].Detail)
	}
}
