// stripe-init.js Payment Element を confirm ページに描画する初期化スクリプト
// このファイルは自ドメインから配信され、CSPで許可されます。
(function () {
  const el = document.currentScript;
  const clientSecret = el && el.dataset ? el.dataset.clientSecret : null;
  const pubMeta = document.querySelector('meta[name="stripe-publishable-key"]');
  const pubKey = pubMeta ? pubMeta.getAttribute('content') : null;
  if (!clientSecret || !pubKey) {
    return;
  }
  function boot() {
    if (typeof Stripe !== 'function') {
      window.setTimeout(boot, 50);
      return;
    }
    const stripe = Stripe(pubKey);
    const elements = stripe.elements({ clientSecret: clientSecret });
    const payment = elements.create('payment');
    payment.mount('#payment-element');
    const btn = document.getElementById('pay-btn');
    const errBox = document.getElementById('pay-error');
    btn.addEventListener('click', async function () {
      btn.disabled = true;
      const returnURL = window.location.origin + '/complete/' + window.location.pathname.split('/').pop();
      const { error } = await stripe.confirmPayment({
        elements: elements,
        confirmParams: { return_url: returnURL },
      });
      if (error) {
        errBox.textContent = error.message || '決済に失敗しました';
        btn.disabled = false;
      }
    });
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
