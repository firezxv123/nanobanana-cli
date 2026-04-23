package nanobanana

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nanobanana-cli/browser"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const geminiURL = "https://gemini.google.com/"

// Watermark-removal assets and algorithm are adapted from MIT-licensed
// gemini-watermark-remover / GeminiWatermarkTool projects. See THIRD_PARTY_NOTICES.md.
//
//go:embed watermark_assets/bg_48.png
var watermarkBG48 []byte

//go:embed watermark_assets/bg_96.png
var watermarkBG96 []byte

// Result is the JSON payload returned by `gen`.
type Result struct {
	Prompt     string   `json:"prompt"`
	Refs       []string `json:"refs,omitempty"`
	Full       string   `json:"full"`
	Thumb      string   `json:"thumb"`
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	ThumbWidth int      `json:"thumb_width"`
	ElapsedMS  int64    `json:"elapsed_ms"`
}

// Options drive a single image generation.
type Options struct {
	Prompt          string
	Refs            []string
	OutDir          string
	ThumbWidth      int
	Timeout         time.Duration
	RemoveWatermark bool
}

// Gen orchestrates:
//
//	navigate → fill prompt → submit → wait for inline image →
//	install fetch-hook that breaks Gemini's download chain at step 3 →
//	click "Download full-size" (hook captures final URL, prevents Chrome's
//	download manager from firing — no native save dialog) →
//	fetch the real PNG from evaluate() → save full + thumbnail.
//
// The inline <img> in the chat is a 1024×559 display thumbnail; the real
// original (e.g. 2816×1536) is only served through Gemini's 4-hop download
// chain and is never rendered in the DOM.
func Gen(c *browser.Client, opts Options) (*Result, error) {
	start := time.Now()
	if opts.Prompt == "" {
		return nil, fmt.Errorf("prompt is empty")
	}
	if opts.OutDir == "" {
		opts.OutDir = "."
	}
	if opts.ThumbWidth <= 0 {
		opts.ThumbWidth = 256
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 300 * time.Second
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir output dir: %w", err)
	}

	if err := c.Navigate(geminiURL, true); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}
	if err := waitTextbox(c, 15*time.Second); err != nil {
		return nil, err
	}
	if err := injectPrompt(c, opts.Prompt); err != nil {
		return nil, err
	}
	refPaths, err := pasteReferences(c, opts.Refs, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if err := clickSend(c); err != nil {
		return nil, err
	}
	if err := waitForDisplayedImage(c, opts.Timeout); err != nil {
		return nil, err
	}

	// Install the fetch-hook that breaks Gemini's download chain. The chain is:
	//   POST c8o8Fe                                → JSON, body contains gg-dl URL
	//   GET  lh3.../gg-dl/...?alr=yes              → text/plain = fife URL
	//   GET  work.fife.usercontent.google.com/...  → text/plain = final lh3 URL  ← INTERCEPTED
	//   (Chrome would navigate to the final URL and save via its download
	//    manager, popping up a save-as dialog — we prevent that by returning
	//    an empty response for step 3, then fetching step 4 ourselves via
	//    fetch(), which stays in the JS realm with no download manager.)
	if err := installDownloadHook(c); err != nil {
		return nil, err
	}

	if err := clickDownload(c); err != nil {
		return nil, err
	}

	pngBytes, err := fetchInterceptedImage(c, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if opts.RemoveWatermark {
		pngBytes, err = removeNanoBananaWatermark(pngBytes)
		if err != nil {
			return nil, fmt.Errorf("remove watermark: %w", err)
		}
	}
	w, h, err := imageDimensions(pngBytes)
	if err != nil {
		return nil, fmt.Errorf("parse downloaded image: %w", err)
	}

	stem := time.Now().Format("20060102-150405")
	fullPath := filepath.Join(opts.OutDir, stem+"-full.png")
	thumbPath := filepath.Join(opts.OutDir, stem+"-thumb.png")

	if err := os.WriteFile(fullPath, pngBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write full: %w", err)
	}
	if err := writeThumbnail(pngBytes, thumbPath, opts.ThumbWidth); err != nil {
		return nil, fmt.Errorf("write thumb: %w", err)
	}

	absFull, _ := filepath.Abs(fullPath)
	absThumb, _ := filepath.Abs(thumbPath)
	return &Result{
		Prompt:     opts.Prompt,
		Refs:       refPaths,
		Full:       absFull,
		Thumb:      absThumb,
		Width:      w,
		Height:     h,
		ThumbWidth: opts.ThumbWidth,
		ElapsedMS:  time.Since(start).Milliseconds(),
	}, nil
}

// --- browser-side helpers ---

func waitTextbox(c *browser.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const code = `(function(){
		const tb = document.querySelector('div[contenteditable="true"][role="textbox"]');
		return { ok: !!tb };
	})()`
	for time.Now().Before(deadline) {
		var out struct {
			OK bool `json:"ok"`
		}
		if err := c.EvaluateValue(code, &out); err == nil && out.OK {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for Gemini prompt textbox")
}

func injectPrompt(c *browser.Client, prompt string) error {
	encoded, _ := json.Marshal(prompt)
	code := fmt.Sprintf(`(function(){
		const tb = document.querySelector('div[contenteditable="true"][role="textbox"]');
		if (!tb) return { ok: false, err: 'textbox_not_found' };
		tb.focus();
		document.execCommand('selectAll', false, null);
		document.execCommand('insertText', false, %s);
		return { ok: true };
	})()`, string(encoded))
	var out struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	if err := c.EvaluateValue(code, &out); err != nil {
		return fmt.Errorf("inject prompt: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("inject prompt failed: %s", out.Err)
	}
	return nil
}

func clickSend(c *browser.Client) error {
	const code = `(function(){
		const selectors = ['button.send-button','button[aria-label="发送"]','button[aria-label="Send"]'];
		for (const sel of selectors) {
			const b = document.querySelector(sel);
			if (b && !b.disabled) { b.click(); return { ok: true }; }
		}
		return { ok: false, err: 'send_button_not_found' };
	})()`
	var out struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	if err := c.EvaluateValue(code, &out); err != nil {
		return fmt.Errorf("click send: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("click send failed: %s", out.Err)
	}
	return nil
}

func pasteReferences(c *browser.Client, refs []string, timeout time.Duration) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	absRefs := make([]string, 0, len(refs))
	for i, ref := range refs {
		refBytes, err := os.ReadFile(ref)
		if err != nil {
			return nil, fmt.Errorf("read ref %q: %w", ref, err)
		}
		mimeType := detectReferenceMIME(ref, refBytes)
		if !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("ref %q is not an image (detected %s)", ref, mimeType)
		}
		absRef, err := filepath.Abs(ref)
		if err != nil {
			return nil, fmt.Errorf("resolve ref %q: %w", ref, err)
		}
		fileName := fmt.Sprintf("ref-%02d%s", i+1, strings.ToLower(filepath.Ext(absRef)))
		if filepath.Ext(fileName) == "" {
			fileName += mimeExtension(mimeType)
		}
		if err := pasteReference(c, fileName, mimeType, refBytes); err != nil {
			return nil, err
		}
		if err := waitForReferenceReady(c, fileName, i+1, timeout); err != nil {
			return nil, err
		}
		absRefs = append(absRefs, absRef)
	}
	return absRefs, nil
}

func pasteReference(c *browser.Client, fileName, mimeType string, refBytes []byte) error {
	encodedName, _ := json.Marshal(fileName)
	encodedMIME, _ := json.Marshal(mimeType)
	encodedBase64, _ := json.Marshal(base64.StdEncoding.EncodeToString(refBytes))
	code := fmt.Sprintf(`(function(){
		const tb = document.querySelector('div[contenteditable="true"][role="textbox"]') || document.querySelector('[contenteditable="true"][role="textbox"]');
		if (!tb) return { ok: false, err: 'textbox_not_found' };
		const fileName = %s;
		const mimeType = %s;
		const b64 = %s;
		const bin = atob(b64);
		const bytes = new Uint8Array(bin.length);
		for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
		const file = new File([bytes], fileName, { type: mimeType });
		const dt = new DataTransfer();
		dt.items.add(file);
		tb.focus();
		const ev = new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true });
		return { ok: true, handled: !tb.dispatchEvent(ev) };
	})()`, string(encodedName), string(encodedMIME), string(encodedBase64))
	var out struct {
		OK      bool   `json:"ok"`
		Handled bool   `json:"handled"`
		Err     string `json:"err"`
	}
	if err := c.EvaluateValue(code, &out); err != nil {
		return fmt.Errorf("paste ref %q: %w", fileName, err)
	}
	if !out.OK {
		return fmt.Errorf("paste ref %q failed: %s", fileName, out.Err)
	}
	return nil
}

func waitForReferenceReady(c *browser.Client, fileName string, expectedCount int, timeout time.Duration) error {
	encodedName, _ := json.Marshal(fileName)
	code := fmt.Sprintf(`(function(){
		const bodyText = document.body && document.body.innerText ? document.body.innerText : '';
		const loading = bodyText.includes('正在加载图片') || bodyText.includes('Uploading image') || bodyText.includes('Loading image');
		const duplicate = bodyText.includes('你已上传过名为') && bodyText.includes(%s);
		const sendSelectors = ['button.send-button','button[aria-label="发送"]','button[aria-label="Send"]'];
		const sendReady = sendSelectors.some(sel => { const b = document.querySelector(sel); return b && !b.disabled; });
		const inputField = document.querySelector('.text-input-field.with-file-preview') || document.querySelector('.text-input-field');
		const attachmentRoot = inputField ? (inputField.querySelector('.attachment-preview-wrapper') || inputField.querySelector('.uploader-file-preview-container') || inputField) : null;
		const previewChips = attachmentRoot ? attachmentRoot.querySelectorAll('uploader-file-preview, .file-preview-chip, .file-preview-container') : [];
		const loadedPreviewImages = attachmentRoot ? [...attachmentRoot.querySelectorAll('img[data-test-id="image-preview"], img[data-test-id="uploaded-img"], img.preview-image')].filter(img => img.complete && img.naturalWidth > 0) : [];
		const removeButtons = attachmentRoot ? attachmentRoot.querySelectorAll('[data-test-id="cancel-button"], button.cancel-button[aria-label*="移除文件"], button.cancel-button[aria-label*="Remove file"]') : [];
		const attachmentCount = Math.min(loadedPreviewImages.length, Math.max(previewChips.length, removeButtons.length, loadedPreviewImages.length));
		return { loading, duplicate, sendReady, attachmentCount, chipCount: previewChips.length, loadedImageCount: loadedPreviewImages.length };
	})()`, string(encodedName))
	deadline := time.Now().Add(timeout)
	stable := 0
	for time.Now().Before(deadline) {
		var out struct {
			Loading          bool `json:"loading"`
			Duplicate        bool `json:"duplicate"`
			SendReady        bool `json:"sendReady"`
			AttachmentCount  int  `json:"attachmentCount"`
			ChipCount        int  `json:"chipCount"`
			LoadedImageCount int  `json:"loadedImageCount"`
		}
		if err := c.EvaluateValue(code, &out); err != nil {
			return fmt.Errorf("wait for ref %q: %w", fileName, err)
		}
		if out.Duplicate {
			return fmt.Errorf("Gemini rejected duplicate ref name %q", fileName)
		}
		if !out.Loading && out.SendReady && out.ChipCount >= expectedCount && out.LoadedImageCount >= expectedCount && out.AttachmentCount >= expectedCount {
			stable++
			if stable >= 3 {
				time.Sleep(1200 * time.Millisecond)
				return nil
			}
		} else {
			stable = 0
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for ref %q to finish loading", fileName)
}

func detectReferenceMIME(path string, refBytes []byte) string {
	mimeType := http.DetectContentType(refBytes)
	if strings.HasPrefix(mimeType, "image/") {
		return mimeType
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	default:
		return mimeType
	}
}

func mimeExtension(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	default:
		return ".img"
	}
}

func waitForDisplayedImage(c *browser.Client, timeout time.Duration) error {
	const code = `(function(){
		const img = document.querySelector('generated-image img, .generated-image img, single-image img');
		const ready = !!(img && img.complete && img.naturalWidth > 0);
		const bodyText = document.body && document.body.innerText ? document.body.innerText : '';
		const lower = bodyText.toLowerCase();
		const rateLimited = [
			'too many requests',
			'rate limit',
			'limit reached',
			"you've reached your limit",
			'try again later',
			'please wait',
			'请求过于频繁',
			'请求太频繁',
			'操作过于频繁',
			'稍后再试',
			'请稍后',
			'达到上限'
		].some(s => lower.includes(s.toLowerCase()));
		return { ready, rateLimited };
	})()`
	deadline := time.Now().Add(timeout)
	rateLimitedStable := 0
	for time.Now().Before(deadline) {
		var out struct {
			Ready       bool `json:"ready"`
			RateLimited bool `json:"rateLimited"`
		}
		if err := c.EvaluateValue(code, &out); err != nil {
			return fmt.Errorf("poll image: %w", err)
		}
		if out.Ready {
			return nil
		}
		if out.RateLimited {
			rateLimitedStable++
			if rateLimitedStable >= 3 {
				return fmt.Errorf("Gemini indicates requests are too frequent; wait a while and retry")
			}
		} else {
			rateLimitedStable = 0
		}
		time.Sleep(1 * time.Second)
	}
	if reason, err := classifyNoImageResponse(c); err == nil && reason != "" {
		return fmt.Errorf("%s", reason)
	}
	return fmt.Errorf("timeout waiting for generated image; strengthen the prompt so Gemini clearly generates an image")
}

func classifyNoImageResponse(c *browser.Client) (string, error) {
	const code = `(function(){
		const textOf = el => (el && el.innerText ? el.innerText.trim() : '');
		const bodyText = textOf(document.body);
		const lower = bodyText.toLowerCase();
		const rateLimited = [
			'too many requests',
			'rate limit',
			'limit reached',
			"you've reached your limit",
			'try again later',
			'please wait',
			'请求过于频繁',
			'请求太频繁',
			'操作过于频繁',
			'稍后再试',
			'请稍后',
			'达到上限'
		].some(s => lower.includes(s.toLowerCase()));
		const selectors = [
			'message-content',
			'model-response',
			'.model-response-text',
			'.response-container',
			'.markdown'
		];
		const candidates = selectors.flatMap(sel => [...document.querySelectorAll(sel)])
			.map(textOf)
			.filter(t => t.length >= 20);
		return { rateLimited, hasTextResponse: candidates.length > 0 };
	})()`
	var out struct {
		RateLimited     bool `json:"rateLimited"`
		HasTextResponse bool `json:"hasTextResponse"`
	}
	if err := c.EvaluateValue(code, &out); err != nil {
		return "", err
	}
	if out.RateLimited {
		return "Gemini indicates requests are too frequent; wait a while and retry", nil
	}
	if out.HasTextResponse {
		return "Gemini returned text instead of an image; strengthen the prompt with an explicit image-generation instruction", nil
	}
	return "", nil
}

func clickDownload(c *browser.Client) error {
	const code = `(function(){
		const b = document.querySelector('[data-test-id="download-generated-image-button"]');
		if (!b) return { ok: false, err: 'download_button_not_found' };
		b.click();
		return { ok: true };
	})()`
	var out struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	if err := c.EvaluateValue(code, &out); err != nil {
		return fmt.Errorf("click download: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("click download failed: %s", out.Err)
	}
	return nil
}

// installDownloadHook wraps window.fetch so the step-3 response (which
// contains the final image URL as plain text) is captured into
// window.__nbFinalURL and then replaced with an empty 200 response. This
// breaks the chain client-side — Gemini's next step becomes a no-op and
// Chrome never sees a navigation to a Content-Disposition response, so no
// save dialog fires.
func installDownloadHook(c *browser.Client) error {
	const code = `(function(){
		if (window.__nbHookV3) return { ok: true, already: true };
		window.__nbHookV3 = true;
		window.__nbFinalURL = null;
		window.__nbFinalURLAt = 0;
		if (!window.__nbOrigFetch) window.__nbOrigFetch = window.fetch;
		const origFetch = window.__nbOrigFetch;
		window.fetch = async function(input, init){
			const url = typeof input === 'string' ? input : (input && input.url) || '';
			if (url.includes('work.fife.usercontent.google.com/rd-gg-dl/')) {
				const resp = await origFetch.apply(this, arguments);
				try {
					const text = await resp.clone().text();
					window.__nbFinalURL = (text || '').trim();
					window.__nbFinalURLAt = Date.now();
				} catch (e) { /* ignore */ }
				return new Response('', { status: 200, statusText: 'OK',
					headers: { 'content-type': 'text/plain' } });
			}
			return origFetch.apply(this, arguments);
		};
		return { ok: true };
	})()`
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.EvaluateValue(code, &out); err != nil {
		return fmt.Errorf("install download hook: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("install download hook: unknown failure")
	}
	return nil
}

// fetchInterceptedImage polls for window.__nbFinalURL (set by the step-3
// hook), then fetches the real PNG bytes via evaluate() and returns them.
// The fetch stays in the JS realm, so Chrome's download manager is never
// invoked and no save-as dialog appears.
func fetchInterceptedImage(c *browser.Client, timeout time.Duration) ([]byte, error) {
	const pollCode = `(function(){ return { url: window.__nbFinalURL || '', at: window.__nbFinalURLAt || 0 }; })()`
	const fetchCode = `(async function(){
		const u = window.__nbFinalURL;
		if (!u) return { ok: false, err: 'no_final_url' };
		try {
			const r = await fetch(u);
			if (!r.ok) return { ok: false, err: 'fetch_failed', status: r.status };
			const blob = await r.blob();
			const buf = await blob.arrayBuffer();
			const u8 = new Uint8Array(buf);
			let s = '';
			const chunk = 32768;
			for (let i = 0; i < u8.length; i += chunk) {
				s += String.fromCharCode.apply(null, u8.subarray(i, i + chunk));
			}
			return { ok: true, contentType: blob.type, size: blob.size, base64: btoa(s) };
		} catch (e) { return { ok: false, err: String(e).slice(0, 300) }; }
	})()`

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var poll struct {
			URL string `json:"url"`
			At  int64  `json:"at"`
		}
		if err := c.EvaluateValue(pollCode, &poll); err != nil {
			return nil, fmt.Errorf("poll final url: %w", err)
		}
		if poll.URL != "" {
			var r struct {
				OK          bool   `json:"ok"`
				Err         string `json:"err"`
				Status      int    `json:"status"`
				ContentType string `json:"contentType"`
				Size        int    `json:"size"`
				Base64      string `json:"base64"`
			}
			if err := c.EvaluateValue(fetchCode, &r); err != nil {
				return nil, fmt.Errorf("fetch intercepted url: %w", err)
			}
			if !r.OK {
				return nil, fmt.Errorf("fetch intercepted url failed: %s (status=%d)", r.Err, r.Status)
			}
			if !strings.HasPrefix(r.ContentType, "image/") {
				return nil, fmt.Errorf("unexpected content-type: %s (size=%d)", r.ContentType, r.Size)
			}
			pngBytes, err := base64.StdEncoding.DecodeString(r.Base64)
			if err != nil {
				return nil, fmt.Errorf("base64 decode: %w", err)
			}
			return pngBytes, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil, fmt.Errorf("timeout waiting for download-chain URL (did Gemini change its download flow?)")
}

// imageDimensions reads width/height without fully decoding pixel data.
func imageDimensions(b []byte) (int, int, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// --- local image handling ---

type watermarkConfig struct {
	logoSize     int
	marginRight  int
	marginBottom int
}

func removeNanoBananaWatermark(imageBytes []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	sb := src.Bounds()
	width, height := sb.Dx(), sb.Dy()
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("source image has invalid dimensions")
	}

	cfg := detectWatermarkConfig(width, height)
	if width < cfg.logoSize+cfg.marginRight || height < cfg.logoSize+cfg.marginBottom {
		return encodePNG(src)
	}

	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dst.Set(x, y, src.At(sb.Min.X+x, sb.Min.Y+y))
		}
	}

	alphaMap, err := watermarkAlphaMap(cfg.logoSize)
	if err != nil {
		return nil, err
	}
	removeWatermarkPixels(dst, alphaMap, cfg)
	return encodePNG(dst)
}

func detectWatermarkConfig(width, height int) watermarkConfig {
	if width > 1024 && height > 1024 {
		return watermarkConfig{logoSize: 96, marginRight: 64, marginBottom: 64}
	}
	return watermarkConfig{logoSize: 48, marginRight: 32, marginBottom: 32}
}

func watermarkAlphaMap(size int) ([]float64, error) {
	var bgBytes []byte
	switch size {
	case 48:
		bgBytes = watermarkBG48
	case 96:
		bgBytes = watermarkBG96
	default:
		return nil, fmt.Errorf("unsupported watermark size: %d", size)
	}
	bg, _, err := image.Decode(bytes.NewReader(bgBytes))
	if err != nil {
		return nil, fmt.Errorf("decode watermark background: %w", err)
	}
	alphaMap := make([]float64, size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			r, g, b, _ := bg.At(bg.Bounds().Min.X+x, bg.Bounds().Min.Y+y).RGBA()
			maxChannel := max(max(int(r>>8), int(g>>8)), int(b>>8))
			alphaMap[y*size+x] = float64(maxChannel) / 255.0
		}
	}
	return alphaMap, nil
}

func removeWatermarkPixels(img *image.NRGBA, alphaMap []float64, cfg watermarkConfig) {
	const (
		alphaThreshold = 0.002
		maxAlpha       = 0.99
		logoValue      = 255.0
	)
	x0 := img.Bounds().Dx() - cfg.marginRight - cfg.logoSize
	y0 := img.Bounds().Dy() - cfg.marginBottom - cfg.logoSize
	for row := 0; row < cfg.logoSize; row++ {
		for col := 0; col < cfg.logoSize; col++ {
			alpha := alphaMap[row*cfg.logoSize+col]
			if alpha < alphaThreshold {
				continue
			}
			if alpha > maxAlpha {
				alpha = maxAlpha
			}
			pixOffset := img.PixOffset(x0+col, y0+row)
			oneMinusAlpha := 1.0 - alpha
			for channel := 0; channel < 3; channel++ {
				watermarked := float64(img.Pix[pixOffset+channel])
				original := (watermarked - alpha*logoValue) / oneMinusAlpha
				img.Pix[pixOffset+channel] = uint8(math.Max(0, math.Min(255, math.Round(original))))
			}
		}
	}
}

func encodePNG(src image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

func writeThumbnail(imageBytes []byte, path string, width int) error {
	src, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	sb := src.Bounds()
	if sb.Dx() == 0 {
		return fmt.Errorf("source image has zero width")
	}
	// Preserve aspect; clamp height to ≥1 for extreme aspect ratios.
	height := max(width*sb.Dy()/sb.Dx(), 1)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Over, nil)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, dst)
}
