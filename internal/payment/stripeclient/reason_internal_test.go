package stripeclient

import "testing"

// TestStripeRefundReason Stripeのenum値だけが渡り、自由記述はnil(省略)になることを検証する
func TestStripeRefundReason(t *testing.T) {
	for _, valid := range []string{"duplicate", "fraudulent", "requested_by_customer"} {
		got := stripeRefundReason(valid)
		if got == nil || *got != valid {
			t.Fatalf("enum値%qが渡らない: %v", valid, got)
		}
	}
	for _, free := range []string{"", "顧客要望", "動作検証", "other"} {
		if got := stripeRefundReason(free); got != nil {
			t.Fatalf("自由記述%qがStripeへ渡ってしまう: %v", free, *got)
		}
	}
}
