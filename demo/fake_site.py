# Test copy of a visa calendar for the demo. Slots are simulated; nothing here is real.
import http.server, json, sys, threading
SLOTS = {"p1": [], "p2": [], "p3": [], "p4": [], "p5": []}
lock = threading.Lock()
class H(http.server.SimpleHTTPRequestHandler):
    def _json(self, obj):
        body = json.dumps(obj).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.send_header("Cache-Control", "no-store")
        self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        if self.path.startswith("/open"):
            with lock: SLOTS["p2"] = ["12/18/2026", "01/07/2027"]
            return self._json({"ok": True})
        pid = self.path.split("postId=")[-1]
        with lock: days = list(SLOTS.get(pid, []))
        self._json({"ScheduleDays": [{"ID": i, "Date": d} for i, d in enumerate(days)]})
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", 8091), lambda *a: H(*a, directory=sys.argv[1])).serve_forever()
