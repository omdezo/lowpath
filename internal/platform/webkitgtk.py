# Lowpath's app window on Linux: the UI in a WebKitGTK view, run by the
# system's Python with PyGObject, so the app itself needs no GTK. Started by
# internal/platform (desktop_xdg.go) with the URL in LOWPATH_URL. Exit
# status 3 means no WebKitGTK is installed, 4 that there is no display;
# closing the window, or SIGTERM, exits with 0.
import os
import signal
import sys

try:
    import gi
except ImportError:
    sys.exit(3)

URL = os.environ.pop("LOWPATH_URL", "")
ICON = os.environ.get("LOWPATH_ICON", "")
TITLE = "Lowpath"
SIZE = (1100, 720)
MIN_SIZE = (920, 600)


def load():
    # GTK 4 with WebKitGTK 6.0 (Fedora 39+, Ubuntu 24.04+), then GTK 3
    # with WebKit2GTK 4.1 or 4.0 (older releases). The WebKit version is
    # required first: require_version records a version as soon as it is
    # available, and a Gtk 4.0 recorded before WebKit 6.0 turns out to be
    # missing would make every GTK 3 fallback fail with "already requires".
    for gtk, webkit, name in (("4.0", "6.0", "WebKit"), ("3.0", "4.1", "WebKit2"), ("3.0", "4.0", "WebKit2")):
        try:
            gi.require_version(name, webkit)
            gi.require_version("Gtk", gtk)
            from gi.repository import GLib, Gtk  # noqa: F401
            mod = __import__("gi.repository", fromlist=[name])
            return gtk, getattr(mod, name)
        except (ValueError, ImportError):
            continue
    return None, None


def main():
    if not URL:
        sys.exit(2)
    gtk, webkit = load()
    if webkit is None:
        sys.exit(3)
    from gi.repository import Gdk, GLib, Gtk

    # No display to show a window on (a TTY or ssh session): exit, so the
    # app opens the browser or prints the address instead of waiting for a
    # window nobody can see.
    if Gdk.Display.get_default() is None:
        sys.exit(4)

    # The Wayland app id and X11 class: they match lowpath.desktop, so the
    # dock shows Lowpath's name and icon.
    GLib.set_prgname("lowpath")
    GLib.set_application_name(TITLE)

    view = webkit.WebView()
    view.load_uri(URL)
    loop = GLib.MainLoop()
    if gtk == "4.0":
        win = Gtk.Window(title=TITLE)
        win.set_default_size(*SIZE)
        win.set_size_request(*MIN_SIZE)
        win.set_icon_name("lowpath")
        win.set_child(view)
        win.connect("close-request", lambda *_: loop.quit() or False)
        win.present()
    else:
        win = Gtk.Window(title=TITLE)
        win.set_default_size(*SIZE)
        win.set_size_request(*MIN_SIZE)
        if ICON:
            try:
                win.set_icon_from_file(ICON)
            except GLib.Error:
                pass
        win.add(view)
        win.connect("destroy", lambda *_: loop.quit())
        win.show_all()
    GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signal.SIGTERM, lambda *_: loop.quit() or False)
    GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signal.SIGINT, lambda *_: loop.quit() or False)
    loop.run()


# Run as a script (python3 -c); the tests load it as a module to check
# load() against a stand-in gi.
if __name__ == "__main__":
    main()
