package chatgptimage

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/sha3"
)

const (
	defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	maxIterations    = 500000
)

var (
	screenSizes  = []int{3000, 4000, 3120, 4160}
	coreCounts   = []int{8, 16, 24, 32}
	documentKey  = []string{"_reactListeningo743lnnpvdg", "location"}
	navigatorKey = []string{
		"registerProtocolHandler\u2212function registerProtocolHandler() { [native code] }",
		"storage\u2212[object StorageManager]",
		"locks\u2212[object LockManager]",
		"appCodeName\u2212Mozilla",
		"permissions\u2212[object Permissions]",
		"share\u2212function share() { [native code] }",
		"webdriver\u2212false",
		"managed\u2212[object NavigatorManagedData]",
		"canShare\u2212function canShare() { [native code] }",
		"vendor\u2212Google Inc.",
		"vendor\u2212Google Inc.",
		"mediaDevices\u2212[object MediaDevices]",
		"vibrate\u2212function vibrate() { [native code] }",
		"storageBuckets\u2212[object StorageBucketManager]",
		"mediaCapabilities\u2212[object MediaCapabilities]",
		"getGamepads\u2212function getGamepads() { [native code] }",
		"bluetooth\u2212[object Bluetooth]",
		"share\u2212function share() { [native code] }",
		"cookieEnabled\u2212true",
		"virtualKeyboard\u2212[object VirtualKeyboard]",
		"product\u2212Gecko",
		"mediaDevices\u2212[object MediaDevices]",
		"canShare\u2212function canShare() { [native code] }",
		"getGamepads\u2212function getGamepads() { [native code] }",
		"product\u2212Gecko",
		"xr\u2212[object XRSystem]",
		"clipboard\u2212[object Clipboard]",
		"storageBuckets\u2212[object StorageBucketManager]",
		"unregisterProtocolHandler\u2212function unregisterProtocolHandler() { [native code] }",
		"productSub\u221220030107",
		"login\u2212[object NavigatorLogin]",
		"vendorSub\u2212",
		"login\u2212[object NavigatorLogin]",
		"getInstalledRelatedApps\u2212function getInstalledRelatedApps() { [native code] }",
		"mediaDevices\u2212[object MediaDevices]",
		"locks\u2212[object LockManager]",
		"webkitGetUserMedia\u2212function webkitGetUserMedia() { [native code] }",
		"vendor\u2212Google Inc.",
		"xr\u2212[object XRSystem]",
		"mediaDevices\u2212[object MediaDevices]",
		"virtualKeyboard\u2212[object VirtualKeyboard]",
		"virtualKeyboard\u2212[object VirtualKeyboard]",
		"appName\u2212Netscape",
		"storageBuckets\u2212[object StorageBucketManager]",
		"presentation\u2212[object Presentation]",
		"onLine\u2212true",
		"mimeTypes\u2212[object MimeTypeArray]",
		"credentials\u2212[object CredentialsContainer]",
		"presentation\u2212[object Presentation]",
		"getGamepads\u2212function getGamepads() { [native code] }",
		"vendorSub\u2212",
		"virtualKeyboard\u2212[object VirtualKeyboard]",
		"serviceWorker\u2212[object ServiceWorkerContainer]",
		"xr\u2212[object XRSystem]",
		"product\u2212Gecko",
		"keyboard\u2212[object Keyboard]",
		"gpu\u2212[object GPU]",
		"getInstalledRelatedApps\u2212function getInstalledRelatedApps() { [native code] }",
		"webkitPersistentStorage\u2212[object DeprecatedStorageQuota]",
		"doNotTrack",
		"clearAppBadge\u2212function clearAppBadge() { [native code] }",
		"presentation\u2212[object Presentation]",
		"serial\u2212[object Serial]",
		"locks\u2212[object LockManager]",
		"requestMIDIAccess\u2212function requestMIDIAccess() { [native code] }",
		"locks\u2212[object LockManager]",
		"requestMediaKeySystemAccess\u2212function requestMediaKeySystemAccess() { [native code] }",
		"vendor\u2212Google Inc.",
		"pdfViewerEnabled\u2212true",
		"language\u2212zh-CN",
		"setAppBadge\u2212function setAppBadge() { [native code] }",
		"geolocation\u2212[object Geolocation]",
		"userAgentData\u2212[object NavigatorUAData]",
		"mediaCapabilities\u2212[object MediaCapabilities]",
		"requestMIDIAccess\u2212function requestMIDIAccess() { [native code] }",
		"getUserMedia\u2212function getUserMedia() { [native code] }",
		"mediaDevices\u2212[object MediaDevices]",
		"webkitPersistentStorage\u2212[object DeprecatedStorageQuota]",
		"sendBeacon\u2212function sendBeacon() { [native code] }",
		"hardwareConcurrency\u221232",
		"credentials\u2212[object CredentialsContainer]",
		"storage\u2212[object StorageManager]",
		"cookieEnabled\u2212true",
		"pdfViewerEnabled\u2212true",
		"windowControlsOverlay\u2212[object WindowControlsOverlay]",
		"scheduling\u2212[object Scheduling]",
		"pdfViewerEnabled\u2212true",
		"hardwareConcurrency\u221232",
		"xr\u2212[object XRSystem]",
		"webdriver\u2212false",
		"getInstalledRelatedApps\u2212function getInstalledRelatedApps() { [native code] }",
		"getInstalledRelatedApps\u2212function getInstalledRelatedApps() { [native code] }",
		"bluetooth\u2212[object Bluetooth]",
	}
	windowKey = []string{
		"0", "window", "self", "document", "name", "location", "customElements",
		"history", "navigation", "locationbar", "menubar", "personalbar",
		"scrollbars", "statusbar", "toolbar", "status", "closed", "frames",
		"length", "top", "opener", "parent", "frameElement", "navigator",
		"origin", "external", "screen", "innerWidth", "innerHeight", "scrollX",
		"pageXOffset", "scrollY", "pageYOffset", "visualViewport", "screenX",
		"screenY", "outerWidth", "outerHeight", "devicePixelRatio",
		"clientInformation", "screenLeft", "screenTop", "styleMedia", "onsearch",
		"isSecureContext", "trustedTypes", "performance", "onappinstalled",
		"onbeforeinstallprompt", "crypto", "indexedDB", "sessionStorage",
		"localStorage", "onbeforexrselect", "onabort", "onbeforeinput",
		"onbeforematch", "onbeforetoggle", "onblur", "oncancel", "oncanplay",
		"oncanplaythrough", "onchange", "onclick", "onclose",
		"oncontentvisibilityautostatechange", "oncontextlost", "oncontextmenu",
		"oncontextrestored", "oncuechange", "ondblclick", "ondrag", "ondragend",
		"ondragenter", "ondragleave", "ondragover", "ondragstart", "ondrop",
		"ondurationchange", "onemptied", "onended", "onerror", "onfocus",
		"onformdata", "oninput", "oninvalid", "onkeydown", "onkeypress",
		"onkeyup", "onload", "onloadeddata", "onloadedmetadata", "onloadstart",
		"onmousedown", "onmouseenter", "onmouseleave", "onmousemove",
		"onmouseout", "onmouseover", "onmouseup", "onmousewheel", "onpause",
		"onplay", "onplaying", "onprogress", "onratechange", "onreset",
		"onresize", "onscroll", "onsecuritypolicyviolation", "onseeked",
		"onseeking", "onselect", "onslotchange", "onstalled", "onsubmit",
		"onsuspend", "ontimeupdate", "ontoggle", "onvolumechange", "onwaiting",
		"onwebkitanimationend", "onwebkitanimationiteration",
		"onwebkitanimationstart", "onwebkittransitionend", "onwheel",
		"onauxclick", "ongotpointercapture", "onlostpointercapture",
		"onpointerdown", "onpointermove", "onpointerrawupdate", "onpointerup",
		"onpointercancel", "onpointerover", "onpointerout", "onpointerenter",
		"onpointerleave", "onselectstart", "onselectionchange",
		"onanimationend", "onanimationiteration", "onanimationstart",
		"ontransitionrun", "ontransitionstart", "ontransitionend",
		"ontransitioncancel", "onafterprint", "onbeforeprint", "onbeforeunload",
		"onhashchange", "onlanguagechange", "onmessage", "onmessageerror",
		"onoffline", "ononline", "onpagehide", "onpageshow", "onpopstate",
		"onrejectionhandled", "onstorage", "onunhandledrejection", "onunload",
		"crossOriginIsolated", "scheduler", "alert", "atob", "blur", "btoa",
		"cancelAnimationFrame", "cancelIdleCallback", "captureEvents",
		"clearInterval", "clearTimeout", "close", "confirm", "createImageBitmap",
		"fetch", "find", "focus", "getComputedStyle", "getSelection",
		"matchMedia", "moveBy", "moveTo", "open", "postMessage", "print",
		"prompt", "queueMicrotask", "releaseEvents", "reportError",
		"requestAnimationFrame", "requestIdleCallback", "resizeBy", "resizeTo",
		"scroll", "scrollBy", "scrollTo", "setInterval", "setTimeout", "stop",
		"structuredClone", "webkitCancelAnimationFrame",
		"webkitRequestAnimationFrame", "chrome", "caches", "cookieStore",
		"ondevicemotion", "ondeviceorientation", "ondeviceorientationabsolute",
		"launchQueue", "documentPictureInPicture", "getScreenDetails",
		"queryLocalFonts", "showDirectoryPicker", "showOpenFilePicker",
		"showSaveFilePicker", "originAgentCluster", "onpageswap",
		"onpagereveal", "credentialless", "speechSynthesis", "onscrollend",
		"webkitRequestFileSystem", "webkitResolveLocalFileSystemURL",
		"sendMsgToSolverCS", "webpackChunk_N_E", "__next_set_public_path__",
		"next", "__NEXT_DATA__", "__SSG_MANIFEST_CB", "__NEXT_P", "_N_E",
		"regeneratorRuntime", "__REACT_INTL_CONTEXT__", "DD_RUM", "_",
		"filterCSS", "filterXSS", "__SEGMENT_INSPECTOR__", "__NEXT_PRELOADREADY",
		"Intercom", "__MIDDLEWARE_MATCHERS", "__STATSIG_SDK__",
		"__STATSIG_JS_SDK__", "__STATSIG_RERENDER_OVERRIDE__",
		"_oaiHandleSessionExpired", "__BUILD_MANIFEST", "__SSG_MANIFEST",
		"__intercomAssignLocation", "__intercomReloadLocation",
	}
)

func buildConfig(userAgent string) []any {
	now := time.Now()
	perfCounter := float64(now.UnixMilli()%1000000) + mrand.Float64()
	epochOffset := float64(now.UnixMilli()) - perfCounter

	return []any{
		screenSizes[mrand.IntN(len(screenSizes))],
		formatBrowserParseTime(now),
		4294705152,
		0,
		userAgent,
		"https://chatgpt.com/backend-api/sentinel/sdk.js",
		"",
		"en-US",
		"en-US,es-US,en,es",
		0,
		navigatorKey[mrand.IntN(len(navigatorKey))],
		documentKey[mrand.IntN(len(documentKey))],
		windowKey[mrand.IntN(len(windowKey))],
		perfCounter,
		uuid.NewString(),
		"",
		coreCounts[mrand.IntN(len(coreCounts))],
		epochOffset,
	}
}

func formatBrowserParseTime(now time.Time) string {
	loc := time.FixedZone("EST", -5*60*60)
	return now.In(loc).Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
}

func solvePoW(seed string, difficulty string) (string, error) {
	config := buildConfig(defaultUserAgent)

	diffBytes, err := hex.DecodeString(difficulty)
	if err != nil {
		return "", fmt.Errorf("invalid difficulty hex %q: %w", difficulty, err)
	}
	diffLen := len(diffBytes)

	part1JSON, _ := json.Marshal(config[:3])
	part4to8JSON, _ := json.Marshal(config[4:9])
	part10JSON, _ := json.Marshal(config[10:])

	staticPart1 := append(part1JSON[:len(part1JSON)-1], ',')
	mid := part4to8JSON[1 : len(part4to8JSON)-1]
	staticPart2 := make([]byte, 0, len(mid)+2)
	staticPart2 = append(staticPart2, ',')
	staticPart2 = append(staticPart2, mid...)
	staticPart2 = append(staticPart2, ',')
	tail := part10JSON[1:]
	staticPart3 := make([]byte, 0, len(tail)+1)
	staticPart3 = append(staticPart3, ',')
	staticPart3 = append(staticPart3, tail...)

	seedBytes := []byte(seed)
	for i := 0; i < maxIterations; i++ {
		iStr := []byte(fmt.Sprintf("%d", i))
		jStr := []byte(fmt.Sprintf("%d", i>>1))

		assembled := make([]byte, 0, len(staticPart1)+len(iStr)+len(staticPart2)+len(jStr)+len(staticPart3))
		assembled = append(assembled, staticPart1...)
		assembled = append(assembled, iStr...)
		assembled = append(assembled, staticPart2...)
		assembled = append(assembled, jStr...)
		assembled = append(assembled, staticPart3...)

		b64 := base64.StdEncoding.EncodeToString(assembled)
		hasher := sha3.New512()
		_, _ = hasher.Write(seedBytes)
		_, _ = hasher.Write([]byte(b64))
		hash := hasher.Sum(nil)

		if bytesLE(hash[:diffLen], diffBytes) {
			return "gAAAAAB" + b64, nil
		}
	}

	fallback := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%q", seed)))
	return "gAAAAABwQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + fallback, nil
}

func bytesLE(a []byte, b []byte) bool {
	for i := range a {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return true
}

func generateRequirementsToken() string {
	config := buildConfig(defaultUserAgent)

	part1JSON, _ := json.Marshal(config[:3])
	part4to8JSON, _ := json.Marshal(config[4:9])
	part10JSON, _ := json.Marshal(config[10:])

	staticPart1 := append(part1JSON[:len(part1JSON)-1], ',')
	mid := part4to8JSON[1 : len(part4to8JSON)-1]
	staticPart2 := make([]byte, 0, len(mid)+2)
	staticPart2 = append(staticPart2, ',')
	staticPart2 = append(staticPart2, mid...)
	staticPart2 = append(staticPart2, ',')
	tail := part10JSON[1:]
	staticPart3 := make([]byte, 0, len(tail)+1)
	staticPart3 = append(staticPart3, ',')
	staticPart3 = append(staticPart3, tail...)

	assembled := make([]byte, 0, len(staticPart1)+1+len(staticPart2)+1+len(staticPart3))
	assembled = append(assembled, staticPart1...)
	assembled = append(assembled, '0')
	assembled = append(assembled, staticPart2...)
	assembled = append(assembled, '0')
	assembled = append(assembled, staticPart3...)

	return "gAAAAAC" + base64.StdEncoding.EncodeToString(assembled)
}
