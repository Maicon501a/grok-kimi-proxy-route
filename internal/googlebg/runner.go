// Package googlebg runs Google's BotGuard VM headless (no browser) to mint the
// signin challenge tokens ("identity-signin-identifier" / "identity-signin-password")
// required by the accounts.google.com v3 batchexecute login flow.
//
// Architecture mirrors BgUtils (bgutils-js): the Google page ships a JS
// interpreter ("bfkj" module) plus an obfuscated program. We execute the
// interpreter in goja with minimal browser shims, initialize the VM with the
// program, then call the async snapshot function.
//
// NOTE: token generation inputs and the interpreter/program acquisition are
// being reverse engineered (see research/google-auth-flow/BATCHEXECUTE-2026-08-19.md).
package googlebg

import (
	"errors"
	"fmt"
	"sync"

	"github.com/dop251/goja"
)

// Runner holds a live BotGuard VM instance.
type Runner struct {
	vm      *goja.Runtime
	mu      sync.Mutex
	snapFn  goja.Callable // asyncSnapshotFunction
	ready   bool
}

// Config carries the BotGuard bootstrap extracted from a signin page.
type Config struct {
	// InterpreterJS is the bfkj interpreter script (defines the VM loader).
	InterpreterJS string
	// Program is the obfuscated botguard program string.
	Program string
	// GlobalName is where the interpreter exposes the VM (e.g. "botguard" → window.botguard.bg).
	GlobalName string
}

// New creates a VM: shims browser globals, evaluates the interpreter, then
// calls vm.a(program, setupCallback, true, nil, telemetry, [[],[]], undefined, false, loggers)
// to obtain the snapshot functions.
func New(cfg Config) (*Runner, error) {
	if cfg.InterpreterJS == "" || cfg.Program == "" {
		return nil, errors.New("googlebg: interpreter and program required")
	}
	vm := goja.New()
	r := &Runner{vm: vm}
	if err := r.installShims(); err != nil {
		return nil, fmt.Errorf("googlebg shims: %w", err)
	}
	if _, err := vm.RunString(cfg.InterpreterJS); err != nil {
		return nil, fmt.Errorf("googlebg interpreter eval: %w", err)
	}
	if err := r.initVM(cfg); err != nil {
		return nil, err
	}
	return r, nil
}

// Snapshot mints a botguard token for the given content binding (challenge).
func (r *Runner) Snapshot(contentBinding map[string]any) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready {
		return "", errors.New("googlebg: VM not initialized")
	}
	// asyncSnapshotFunction(resolve, [contentBinding, undefined, undefined, undefined])
	resultCh := make(chan goja.Value, 1)
	resolve := func(call goja.FunctionCall) goja.Value {
		resultCh <- call.Argument(0)
		return goja.Undefined()
	}
	args := r.vm.ToValue([]any{contentBinding, nil, nil, nil})
	if _, err := r.snapFn(goja.Undefined(), r.vm.ToValue(resolve), args); err != nil {
		return "", fmt.Errorf("googlebg snapshot call: %w", err)
	}
	v := <-resultCh
	s := v.String()
	if s == "" || s == "undefined" {
		return "", errors.New("googlebg: empty snapshot")
	}
	return s, nil
}

// --- internals ---

func (r *Runner) installShims() error {
	shims := `
var window = globalThis;
var self = globalThis;
if (typeof globalThis.navigator === 'undefined') {
  globalThis.navigator = {
    userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36',
    language: 'pt-BR', languages: ['pt-BR','pt','en'],
    platform: 'Win32', hardwareConcurrency: 8, deviceMemory: 8,
    maxTouchPoints: 0, cookieEnabled: true, webdriver: false,
    plugins: { length: 0 }, mimeTypes: { length: 0 }
  };
}
if (typeof globalThis.screen === 'undefined') {
  globalThis.screen = { width: 1280, height: 800, availWidth: 1280, availHeight: 770, colorDepth: 24, pixelDepth: 24 };
}
if (typeof globalThis.location === 'undefined') {
  globalThis.location = { href: 'https://accounts.google.com/v3/signin/identifier', origin: 'https://accounts.google.com', host: 'accounts.google.com', hostname: 'accounts.google.com', protocol: 'https:', pathname: '/v3/signin/identifier', search: '', hash: '' };
}
if (typeof globalThis.document === 'undefined') {
  var mkEl = function(){ return { style:{}, getAttribute:function(){return null}, setAttribute:function(){}, appendChild:function(){}, addEventListener:function(){}, getElementsByTagName:function(){return []}, querySelector:function(){return null}, querySelectorAll:function(){return []} }; };
  globalThis.document = {
    documentElement: mkEl(), head: mkEl(), body: mkEl(),
    createElement: function(){ return mkEl(); },
    getElementsByTagName: function(){ return []; },
    querySelector: function(){ return null; },
    querySelectorAll: function(){ return []; },
    addEventListener: function(){}, removeEventListener: function(){},
    cookie: '', referrer: '', hidden: true, visibilityState: 'hidden',
    readyState: 'complete'
  };
}
if (typeof globalThis.performance === 'undefined') {
  globalThis.performance = { now: function(){ return Date.now() % 100000; }, timeOrigin: Date.now() };
}
if (typeof globalThis.crypto === 'undefined') {
  globalThis.crypto = { getRandomValues: function(arr){ for (var i=0;i<arr.length;i++) arr[i]=Math.floor(Math.random()*256); return arr; } };
}
if (typeof globalThis.atob === 'undefined') {
  globalThis.atob = function(s){ return gojaAtob(s); };
  globalThis.btoa = function(s){ return gojaBtoa(s); };
}
if (typeof globalThis.addEventListener === 'undefined') {
  globalThis.addEventListener = function(){}; globalThis.removeEventListener = function(){};
}
`
	// goja has no atob/btoa — provided from Go side.
	r.vm.Set("gojaAtob", func(s string) string { return atob(s) })
	r.vm.Set("gojaBtoa", func(s string) string { return btoa(s) })
	_, err := r.vm.RunString(shims)
	return err
}

func (r *Runner) initVM(cfg Config) error {
	// Locate the VM object: globalName may be dotted ("botguard.bg").
	vmObj := r.vm.GlobalObject()
	name := cfg.GlobalName
	if name == "" {
		name = "botguard.bg"
	}
	parts := splitDot(name)
	var cur goja.Value = r.vm.GlobalObject().ToObject(r.vm).Get(parts[0])
	for _, p := range parts[1:] {
		if cur == nil || goja.IsUndefined(cur) || goja.IsNull(cur) {
			return fmt.Errorf("googlebg: global %q not found after interpreter eval", name)
		}
		cur = cur.ToObject(r.vm).Get(p)
	}
	if cur == nil || goja.IsUndefined(cur) {
		return fmt.Errorf("googlebg: global %q undefined", name)
	}
	vmObj = cur.ToObject(r.vm)
	initFn, ok := goja.AssertFunction(vmObj.Get("a"))
	if !ok {
		return errors.New("googlebg: VM init function .a not found")
	}

	setupCh := make(chan goja.Value, 1)
	setupCb := func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) > 0 {
			setupCh <- call.Argument(0) // asyncSnapshotFunction
		}
		return goja.Undefined()
	}
	telemetry := func(goja.FunctionCall) goja.Value { return goja.Undefined() }
	loggers := r.vm.NewArray()
	empty2 := r.vm.NewArray()
	empty2.Set("0", r.vm.NewArray())
	empty2.Set("1", r.vm.NewArray())

	ret, err := initFn(vmObj,
		r.vm.ToValue(cfg.Program),
		r.vm.ToValue(setupCb),
		r.vm.ToValue(true),
		goja.Null(), // userInteractionElement
		r.vm.ToValue(telemetry),
		empty2,
		goja.Undefined(),
		r.vm.ToValue(false),
		loggers,
	)
	if err != nil {
		return fmt.Errorf("googlebg: vm.a init failed: %w", err)
	}
	_ = ret
	select {
	case fn := <-setupCh:
		callable, ok := goja.AssertFunction(fn)
		if !ok {
			return errors.New("googlebg: asyncSnapshotFunction not callable")
		}
		r.snapFn = callable
		r.ready = true
	default:
		return errors.New("googlebg: setup callback not invoked synchronously")
	}
	return nil
}

func splitDot(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '.' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	out = append(out, cur)
	return out
}
