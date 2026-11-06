package apps

import (
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/htmlrender"
)

func managementPage(data pageData) string {
	var body strings.Builder
	body.WriteString(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>Mesh temporary apps</title><style>body{font:16px system-ui;background:#101214;color:#eef0f2;margin:0;padding:24px}main{max-width:680px;margin:auto}a{color:#9ddcff}button,a.button{border:0;border-radius:999px;background:#e9edf2;color:#101214;padding:10px 16px;cursor:pointer;display:inline-block;text-decoration:none}code{font-size:16px;overflow-wrap:anywhere}small{color:#bcc4cb}form{display:inline}.error{color:#ffbab4}</style></head><body><main>`)
	if data.Error != "" {
		fmt.Fprintf(&body, `<p class="error">%s</p>`, htmlrender.Text(data.Error))
	}
	writePageContent(&body, data)
	body.WriteString("</main>")
	if data.Code != "" {
		writePairScript(&body, data)
	}
	if data.Confirm != "" {
		writeConfirmScript(&body, data.App.ID)
	}
	body.WriteString("</body></html>")
	return body.String()
}

func writePageContent(body *strings.Builder, data pageData) {
	switch {
	case data.Code != "":
		code := htmlrender.Text(data.Code)
		fmt.Fprintf(body, `<h1>Pair this browser</h1><p>Approve from the Mesh host that owns the app:</p><code>mesh app browser approve HOST %s</code><p>Code: <strong>%s</strong></p><p>Expires in 10 minutes.</p><p id="pair-status" role="status">Waiting for approval. This page will continue automatically.</p><button id="check-approval" type="button">Check now</button><p><a id="restart-pair" href="/pair" hidden>Get a new code</a></p>`, code, code)
	case data.PairStart:
		fmt.Fprintf(body, `<h1>Pair this browser</h1><p>Start pairing, then approve the code on the Mesh host that owns your app.</p><form method="post" action="/pair"><input type="hidden" name="return" value="%s"><button>Start pairing</button></form>`, htmlrender.Text(data.Return))
	case data.Confirm != "":
		writeConfirmation(body, data)
	default:
		body.WriteString(`<h1>Temporary apps</h1><p>Apps expire after 24 hours without app traffic. Expiry deletes their managed source, data and server.</p>`)
		for _, app := range data.Apps {
			writeAppRow(body, app, data.Domain)
		}
		body.WriteString(`<a href="/pair">Pair another owner</a>`)
	}
}

func writeConfirmation(body *strings.Builder, data pageData) {
	const deleteAction = "delete"
	action, id := htmlrender.Text(data.Confirm), htmlrender.Text(data.App.ID)
	fmt.Fprintf(body, `<h1>%s %s</h1><p>Owner controls for <a href="%s">%s</a>.</p><form id="confirm-form" method="post" action="/action"><input type="hidden" name="id" value="%s"><input type="hidden" name="action" value="%s"><input type="hidden" name="csrf" value="%s"><input type="hidden" name="return" value="%s">`, action, id, htmlrender.Href(data.URL), htmlrender.Text(data.URL), id, action, htmlrender.Text(data.CSRF), htmlrender.Text(data.Return))
	if data.Confirm == deleteAction {
		fmt.Fprintf(body, `<p><label>Type %s to confirm <input id="confirm-id" name="confirmation" autocomplete="off" required></label></p>`, id)
	}
	fmt.Fprintf(body, `<p><small>Read the change, then move the pointer or use the keyboard to enable confirmation.</small></p><button id="confirm-action" disabled>Confirm %s</button></form>`, action)
}

func writeAppRow(body *strings.Builder, app Record, domain string) {
	const activeStatus = "active"
	queryID := htmlrender.Query(app.ID)
	fmt.Fprintf(body, `<p><a href="/view?id=%s">https://%s.%s</a> · %s<br><small>Expires %s</small>`, queryID, htmlrender.Text(app.ID), htmlrender.Text(domain), htmlrender.Text(app.Status), htmlrender.Text(app.ExpiresAt.String()))
	if app.Status == activeStatus {
		fmt.Fprintf(body, `<br><a href="/download?id=%s">Download source</a> · <a href="/confirm?id=%s&action=renew">Renew</a> · <a href="/confirm?id=%s&action=delete">Delete</a>`, queryID, queryID, queryID)
	}
	body.WriteString(`</p>`)
}

func writePairScript(body *strings.Builder, data pageData) {
	fmt.Fprintf(body, `<script>
const checkButton = document.getElementById('check-approval');
const status = document.getElementById('pair-status');
const expiresAt = performance.now() +  %d ;
let checking = false;
let timer;
let finished = false;
function expired() {
  finished = true;
  status.textContent = 'This code expired. Get a new code to try again.';
  checkButton.hidden = true;
  document.getElementById('restart-pair').hidden = false;
}
async function checkApproval() {
  if (checking || finished) return;
  clearTimeout(timer);
  if (performance.now() >= expiresAt) { expired(); return; }
  checking = true;
  checkButton.disabled = true;
  const abort = new AbortController();
  const timeout = setTimeout(() => abort.abort(), 10000);
  try {
    const response = await fetch('/pair/status', {credentials: 'same-origin', cache: 'no-store', signal: abort.signal});
    if (response.status === 200) { finished = true; location.replace(%s); return; }
    if (response.status === 410) { expired(); return; }
    status.textContent = response.status === 202
      ? 'Waiting for approval. This page will continue automatically.'
      : 'Could not check approval. Retrying automatically.';
  } catch {
    status.textContent = 'Could not check approval. Retrying automatically.';
  } finally {
    clearTimeout(timeout);
    checking = false;
    checkButton.disabled = false;
    if (!finished) timer = setTimeout(checkApproval, 1500);
  }
}
checkButton.addEventListener('click', checkApproval);
document.addEventListener('visibilitychange', () => { if (!document.hidden) checkApproval(); });
checkApproval();
</script>`, data.PairLifetimeMS, htmlrender.String(data.Return))
}

func writeConfirmScript(body *strings.Builder, id string) {
	fmt.Fprintf(body, `<script>
const confirmButton = document.getElementById('confirm-action');
const confirmForm = document.getElementById('confirm-form');
const confirmID = document.getElementById('confirm-id');
let focusedAt = null;
let deliberate = false;
function activePage() { return document.visibilityState === 'visible' && document.hasFocus(); }
function updateActivation() {
  confirmButton.disabled = !activePage() || focusedAt === null || performance.now() - focusedAt < 750 || !deliberate || (confirmID && confirmID.value !== %s);
}
function resetActivation() {
  deliberate = false;
  focusedAt = activePage() ? performance.now() : null;
  confirmButton.disabled = true;
}
function deliberateInput(event) {
  if (event.isTrusted && activePage() && focusedAt !== null && performance.now() - focusedAt >= 750) {
    deliberate = true;
    updateActivation();
  }
}
window.addEventListener('focus', resetActivation);
window.addEventListener('blur', resetActivation);
window.addEventListener('pageshow', resetActivation);
document.addEventListener('visibilitychange', resetActivation);
document.addEventListener('pointermove', deliberateInput);
document.addEventListener('pointerdown', deliberateInput, true);
document.addEventListener('touchstart', deliberateInput, {capture: true, passive: true});
document.addEventListener('keydown', deliberateInput);
if (confirmID) confirmID.addEventListener('input', updateActivation);
confirmForm.addEventListener('submit', event => {
  updateActivation();
  if (confirmButton.disabled) event.preventDefault();
});
resetActivation();
</script>`, htmlrender.String(id))
}
