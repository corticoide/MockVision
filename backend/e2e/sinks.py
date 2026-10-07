#!/usr/bin/env python3
"""Event targets of the end-to-end test besides HTTP, with the standard
library alone: an MQTT 3.1.1 broker (port 1883), an FTP server in passive
mode (2121) and a mail server (2525). Each logs what it gets, one line per
packet, command or mail, under the directory given; FTP uploads and mails
are stored there too.

    sinks.py DIR
"""

import base64
import os
import socket
import socketserver
import struct
import sys
import threading

DIR = sys.argv[1]
os.makedirs(DIR, exist_ok=True)
LOCK = threading.Lock()


def log(name, line):
    with LOCK, open(os.path.join(DIR, name), "a") as f:
        f.write(line + "\n")


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


# --- MQTT: user cam, password e2e-mqtt ---


def read_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise EOFError
        buf += chunk
    return buf


def read_packet(sock):
    header = read_exact(sock, 1)[0]
    length, mult = 0, 1
    for _ in range(4):
        b = read_exact(sock, 1)[0]
        length += (b & 0x7F) * mult
        if b & 0x80 == 0:
            break
        mult *= 128
    return header, read_exact(sock, length) if length else b""


def write_packet(sock, header, body=b""):
    n, enc = len(body), b""
    while True:
        d = n % 128
        n //= 128
        enc += bytes([d | (0x80 if n else 0)])
        if not n:
            break
    sock.sendall(bytes([header]) + enc + body)


def mqtt_string(b):
    n = struct.unpack(">H", b[:2])[0]
    return b[2 : 2 + n].decode("utf-8", "replace"), b[2 + n :]


class MQTT(socketserver.BaseRequestHandler):
    def handle(self):
        s = self.request
        try:
            while True:
                header, body = read_packet(s)
                kind = header >> 4
                if kind == 1:  # CONNECT
                    _, rest = mqtt_string(body)
                    flags, rest = rest[1], rest[4:]
                    client, rest = mqtt_string(rest)
                    will = ""
                    if flags & 0x04:
                        wt, rest = mqtt_string(rest)
                        wp, rest = mqtt_string(rest)
                        will = f" will={wt}:{wp}"
                    user = pw = ""
                    if flags & 0x80:
                        user, rest = mqtt_string(rest)
                    if flags & 0x40:
                        pw, rest = mqtt_string(rest)
                    ok = user == "cam" and pw == "e2e-mqtt"
                    log("mqtt.log", f"CONNECT client={client} user={user} ok={ok}{will}")
                    write_packet(s, 0x20, bytes([0, 0 if ok else 4]))
                    if not ok:
                        return
                elif kind == 3:  # PUBLISH
                    qos = header >> 1 & 3
                    topic, rest = mqtt_string(body)
                    pid = b""
                    if qos:
                        pid, rest = rest[:2], rest[2:]
                    payload = rest.decode("utf-8", "replace").replace("\n", " ")
                    log("mqtt.log", f"PUBLISH {topic} qos={qos} retain={header & 1} {payload}")
                    if qos == 1:
                        write_packet(s, 0x40, pid)
                    elif qos == 2:
                        write_packet(s, 0x50, pid)
                elif kind == 6:  # PUBREL
                    write_packet(s, 0x70, body)
                elif kind == 12:  # PINGREQ
                    write_packet(s, 0xD0)
                elif kind == 14:  # DISCONNECT
                    log("mqtt.log", "DISCONNECT")
                    return
        except (EOFError, OSError):
            pass


# --- FTP: user cam, password e2e-ftp, files under DIR/ftp ---


class FTP(socketserver.StreamRequestHandler):
    def say(self, line):
        self.wfile.write((line + "\r\n").encode())

    def handle(self):
        root = os.path.join(DIR, "ftp")
        cwd, user, logged, data = "/", "", False, None
        self.say("220 e2e FTP")
        while True:
            raw = self.rfile.readline()
            if not raw:
                return
            line = raw.decode("utf-8", "replace").rstrip("\r\n")
            cmd, _, arg = line.partition(" ")
            cmd = cmd.upper()
            log("ftp.log", "PASS ***" if cmd == "PASS" else line)
            path = os.path.normpath(os.path.join(cwd, arg)) if arg else cwd
            local = os.path.join(root, path.lstrip("/"))
            if cmd == "USER":
                user = arg
                self.say("331 password please")
            elif cmd == "PASS":
                logged = user == "cam" and arg == "e2e-ftp"
                self.say("230 logged in" if logged else "530 login incorrect")
            elif not logged:
                self.say("530 please login")
            elif cmd == "TYPE":
                self.say("200 binary")
            elif cmd == "PWD":
                self.say(f'257 "{cwd}"')
            elif cmd == "CWD":
                if os.path.isdir(local) or path == "/":
                    cwd = path
                    self.say("250 ok")
                else:
                    self.say("550 no such directory")
            elif cmd == "MKD":
                os.makedirs(local, exist_ok=True)
                self.say(f'257 "{path}" created')
            elif cmd in ("EPSV", "PASV"):
                if data:
                    data.close()
                data = socket.socket()
                data.bind((self.connection.getsockname()[0], 0))
                data.listen(1)
                port = data.getsockname()[1]
                if cmd == "EPSV":
                    self.say(f"229 Entering Extended Passive Mode (|||{port}|)")
                else:
                    ip = self.connection.getsockname()[0].replace(".", ",")
                    self.say(f"227 Entering Passive Mode ({ip},{port >> 8},{port & 255})")
            elif cmd == "STOR":
                if not data:
                    self.say("425 use PASV first")
                    continue
                self.say("150 ok to send")
                conn, _ = data.accept()
                os.makedirs(os.path.dirname(local), exist_ok=True)
                size = 0
                with open(local, "wb") as f:
                    while True:
                        chunk = conn.recv(65536)
                        if not chunk:
                            break
                        size += len(chunk)
                        f.write(chunk)
                conn.close()
                data.close()
                data = None
                log("ftp.log", f"STORED {path} {size}")
                self.say("226 transfer complete")
            elif cmd == "QUIT":
                self.say("221 bye")
                return
            else:
                self.say("502 not implemented")


# --- SMTP: user cam, password e2e-mail, mails as DIR/mail-N.eml ---


class SMTP(socketserver.StreamRequestHandler):
    count = 0

    def say(self, line):
        self.wfile.write((line + "\r\n").encode())

    def readline(self):
        raw = self.rfile.readline()
        if not raw:
            raise EOFError
        return raw.decode("utf-8", "replace").rstrip("\r\n")

    def handle(self):
        self.say("220 e2e ESMTP")
        sender, rcpts, user = "", [], ""
        try:
            while True:
                line = self.readline()
                cmd, _, arg = line.partition(" ")
                cmd = cmd.upper()
                log("smtp.log", "AUTH ***" if cmd == "AUTH" else line)
                if cmd == "EHLO":
                    self.say("250-e2e")
                    self.say("250-AUTH PLAIN LOGIN")
                    self.say("250 8BITMIME")
                elif cmd == "AUTH":
                    mech, _, rest = arg.partition(" ")
                    if mech == "PLAIN":
                        parts = base64.b64decode(rest).split(b"\0")
                        u, p = parts[1].decode(), parts[2].decode()
                    else:
                        self.say("334 VXNlcm5hbWU6")
                        u = base64.b64decode(self.readline()).decode()
                        self.say("334 UGFzc3dvcmQ6")
                        p = base64.b64decode(self.readline()).decode()
                    if u == "cam" and p == "e2e-mail":
                        user = u
                        self.say("235 accepted")
                    else:
                        self.say("535 authentication failed")
                elif cmd == "MAIL":
                    sender, rcpts = arg[5:].strip("<>"), []
                    self.say("250 ok")
                elif cmd == "RCPT":
                    rcpts.append(arg[3:].strip("<>"))
                    self.say("250 ok")
                elif cmd == "DATA":
                    if not user:
                        self.say("530 authentication required")
                        continue
                    self.say("354 end with .")
                    lines = []
                    while True:
                        l = self.readline()
                        if l == ".":
                            break
                        lines.append(l[1:] if l.startswith(".") else l)
                    with LOCK:
                        SMTP.count += 1
                        n = SMTP.count
                    with open(os.path.join(DIR, f"mail-{n}.eml"), "w") as f:
                        f.write("\r\n".join(lines) + "\r\n")
                    log("smtp.log", f"MAILED mail-{n}.eml from={sender} to={','.join(rcpts)}")
                    self.say("250 queued")
                elif cmd == "RSET":
                    sender, rcpts = "", []
                    self.say("250 ok")
                elif cmd == "QUIT":
                    self.say("221 bye")
                    return
                else:
                    self.say("502 unknown")
        except (EOFError, OSError):
            pass


if __name__ == "__main__":
    for port, handler in ((1883, MQTT), (2121, FTP), (2525, SMTP)):
        srv = Server(("0.0.0.0", port), handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
    threading.Event().wait()
