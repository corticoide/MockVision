#!/usr/bin/env python3
"""Minimal DHCP server for the end-to-end test.

It offers the addresses of its pool in order, answers by broadcast, never
offers again an address a client declined, and appends one line per message
to a log: the message type, the client's MAC and the address.

    dhcpd.py IFACE SERVER_IP POOL LOG      (POOL: comma-separated addresses)
"""

import socket
import struct
import sys

DISCOVER, OFFER, REQUEST, DECLINE, ACK, NAK, RELEASE = 1, 2, 3, 4, 5, 6, 7
NAMES = {DISCOVER: "DISCOVER", OFFER: "OFFER", REQUEST: "REQUEST", DECLINE: "DECLINE", ACK: "ACK", NAK: "NAK", RELEASE: "RELEASE"}
MAGIC = b"\x63\x82\x53\x63"


def parse_options(data):
    opts, i = {}, 0
    while i < len(data) and data[i] != 255:
        if data[i] == 0:
            i += 1
            continue
        size = data[i + 1]
        opts[data[i]] = data[i + 2 : i + 2 + size]
        i += 2 + size
    return opts


def option(code, value):
    return bytes([code, len(value)]) + value


def main():
    iface, server, pool, log = sys.argv[1], sys.argv[2], sys.argv[3].split(","), sys.argv[4]
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, iface.encode())
    sock.bind(("0.0.0.0", 67))
    offered = {}  # MAC -> address
    declined = set()

    def record(kind, mac, ip):
        with open(log, "a") as f:
            f.write(f"{NAMES.get(kind, kind)} {mac} {ip}\n")

    def reply(request, kind, ip):
        head = struct.pack("!BBBB", 2, 1, 6, 0) + request[4:8] + b"\0\0" + request[10:12]
        head += b"\0" * 4 + socket.inet_aton(ip) + socket.inet_aton(server) + b"\0" * 4 + request[28:44]
        body = head + b"\0" * 192 + MAGIC + option(53, bytes([kind])) + option(54, socket.inet_aton(server))
        if kind != NAK:
            body += option(51, struct.pack("!I", 3600)) + option(1, socket.inet_aton("255.255.255.0"))
            body += option(3, socket.inet_aton(server)) + option(6, socket.inet_aton(server))
        sock.sendto(body + b"\xff", ("255.255.255.255", 68))
        record(kind, mac, ip)

    while True:
        pkt, _ = sock.recvfrom(1500)
        if len(pkt) < 240 or pkt[0] != 1 or pkt[236:240] != MAGIC:
            continue
        mac = ":".join(f"{b:02x}" for b in pkt[28:34])
        opts = parse_options(pkt[240:])
        kind = opts.get(53, b"\0")[0]
        wanted = socket.inet_ntoa(opts[50]) if len(opts.get(50, b"")) == 4 else socket.inet_ntoa(pkt[12:16])
        record(kind, mac, wanted)
        if kind == DISCOVER:
            ip = offered.get(mac) or next((a for a in pool if a not in declined and a not in offered.values()), None)
            if ip:
                offered[mac] = ip
                reply(pkt, OFFER, ip)
        elif kind == REQUEST:
            if offered.get(mac) == wanted:
                reply(pkt, ACK, wanted)
            else:
                reply(pkt, NAK, "0.0.0.0")
        elif kind == DECLINE:
            declined.add(wanted)
            offered.pop(mac, None)
        elif kind == RELEASE:
            offered.pop(mac, None)


if __name__ == "__main__":
    main()
