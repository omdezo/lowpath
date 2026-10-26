// Lowpath's app window on macOS: the UI in a WKWebView, run by osascript's
// JavaScript for Automation bridge to Cocoa, so the app itself needs no cgo.
// Started by internal/platform (desktop_darwin.go) with the URL in
// LOWPATH_URL. Closing the window or Cmd-Q exits with 0.
ObjC.import("Cocoa");
ObjC.import("WebKit");

function run() {
  const env = $.NSProcessInfo.processInfo.environment;
  const get = (k) => {
    const v = env.objectForKey(k);
    return v.isNil() ? "" : ObjC.unwrap(v);
  };
  const url = get("LOWPATH_URL");
  if (!url) return;
  const icon = get("LOWPATH_ICON");

  const app = $.NSApplication.sharedApplication;
  app.setActivationPolicy($.NSApplicationActivationPolicyRegular);
  if (icon) {
    const img = $.NSImage.alloc.initWithContentsOfFile(icon);
    if (!img.isNil()) app.applicationIconImage = img;
  }

  // Cmd-Q, and an Edit menu: WKWebView's copy and paste shortcuts only
  // work through menu items.
  const bar = $.NSMenu.alloc.init;
  const item = (title, action, key) => $.NSMenuItem.alloc.initWithTitleActionKeyEquivalent(title, action, key);
  const appItem = $.NSMenuItem.alloc.init;
  const appMenu = $.NSMenu.alloc.init;
  appMenu.addItem(item("Quit Lowpath", "terminate:", "q"));
  appItem.submenu = appMenu;
  bar.addItem(appItem);
  const editItem = $.NSMenuItem.alloc.init;
  const edit = $.NSMenu.alloc.initWithTitle("Edit");
  [["Undo", "undo:", "z"], ["Redo", "redo:", "Z"], ["Cut", "cut:", "x"], ["Copy", "copy:", "c"],
   ["Paste", "paste:", "v"], ["Select All", "selectAll:", "a"]].forEach(([t, a, k]) => edit.addItem(item(t, a, k)));
  editItem.submenu = edit;
  bar.addItem(editItem);
  app.mainMenu = bar;

  if (!$.LowpathWindowDelegate) {
    ObjC.registerSubclass({
      name: "LowpathWindowDelegate",
      protocols: ["NSWindowDelegate"],
      methods: {
        "windowWillClose:": {
          types: ["void", ["id"]],
          implementation: () => $.NSApplication.sharedApplication.terminate(null),
        },
      },
    });
  }

  const rect = $.NSMakeRect(0, 0, 1100, 720);
  const style = $.NSWindowStyleMaskTitled | $.NSWindowStyleMaskClosable |
    $.NSWindowStyleMaskMiniaturizable | $.NSWindowStyleMaskResizable;
  const win = $.NSWindow.alloc.initWithContentRectStyleMaskBackingDefer(rect, style, $.NSBackingStoreBuffered, false);
  win.title = "Lowpath";
  win.minSize = $.NSMakeSize(920, 600);
  win.releasedWhenClosed = false;
  win.delegate = $.LowpathWindowDelegate.alloc.init;

  const view = $.WKWebView.alloc.initWithFrameConfiguration(rect, $.WKWebViewConfiguration.alloc.init);
  win.contentView = view;
  view.loadRequest($.NSURLRequest.requestWithURL($.NSURL.URLWithString(url)));
  win.center;
  win.makeKeyAndOrderFront(null);
  app.activateIgnoringOtherApps(true);
  app.run;
}
