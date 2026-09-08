"""A tiny RFC 6455 WebSocket client — text frames only.

The exec attach stream carries JSON frames (see ``execframe`` in the Go tree),
so a full WebSocket library is overkill and would break the SDK's
no-dependencies rule. This handles the client handshake, masked text sends,
server frame reassembly, ping/pong, and close.
"""

from __future__ import annotations

import base64
import os
import socket
import ssl
import struct
from urllib.parse import urlparse

_OP_CONT = 0x0
_OP_TEXT = 0x1
_OP_BIN = 0x2
_OP_CLOSE = 0x8
_OP_PING = 0x9
_OP_PONG = 0xA


class WSError(Exception):
    pass


class WSClosed(Exception):
    def __init__(self, code: int = 1000, reason: str = "") -> None:
        self.code = code
        self.reason = reason
        super().__init__(f"websocket closed {code} {reason}".rstrip())


class WebSocket:
    def __init__(self, url: str, *, timeout: float = 15.0) -> None:
        u = urlparse(url)
        secure = u.scheme == "wss"
        host = u.hostname or ""
        port = u.port or (443 if secure else 80)
        path = u.path or "/"
        if u.query:
            path += "?" + u.query

        raw = socket.create_connection((host, port), timeout=timeout)
        if secure:
            ctx = ssl.create_default_context()
            raw = ctx.wrap_socket(raw, server_hostname=host)
        self._sock = raw
        self._buf = b""

        key = base64.b64encode(os.urandom(16)).decode()
        req = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: {host}:{port}\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n"
            "\r\n"
        )
        self._sock.sendall(req.encode())
        header = self._read_until(b"\r\n\r\n")
        status_line = header.split(b"\r\n", 1)[0].decode("latin1")
        if "101" not in status_line:
            raise WSError(f"handshake failed: {status_line}")

    # -- framing ------------------------------------------------------
    def _read_until(self, sep: bytes) -> bytes:
        while sep not in self._buf:
            chunk = self._sock.recv(4096)
            if not chunk:
                raise WSError("connection closed during handshake")
            self._buf += chunk
        head, self._buf = self._buf.split(sep, 1)
        return head + sep

    def _recv_exact(self, n: int) -> bytes:
        while len(self._buf) < n:
            chunk = self._sock.recv(65536)
            if not chunk:
                raise WSClosed(1006, "connection lost")
            self._buf += chunk
        out, self._buf = self._buf[:n], self._buf[n:]
        return out

    def send_text(self, text: str) -> None:
        payload = text.encode("utf-8")
        header = bytearray([0x80 | _OP_TEXT])
        mask = os.urandom(4)
        n = len(payload)
        if n < 126:
            header.append(0x80 | n)
        elif n < 65536:
            header.append(0x80 | 126)
            header += struct.pack("!H", n)
        else:
            header.append(0x80 | 127)
            header += struct.pack("!Q", n)
        header += mask
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self._sock.sendall(bytes(header) + masked)

    def recv_text(self) -> str:
        """Return the next complete text message, transparently answering pings."""
        message = bytearray()
        while True:
            b0, b1 = self._recv_exact(2)
            fin = b0 & 0x80
            opcode = b0 & 0x0F
            length = b1 & 0x7F
            if length == 126:
                (length,) = struct.unpack("!H", self._recv_exact(2))
            elif length == 127:
                (length,) = struct.unpack("!Q", self._recv_exact(8))
            data = self._recv_exact(length) if length else b""

            if opcode == _OP_CLOSE:
                code = 1000
                reason = ""
                if len(data) >= 2:
                    (code,) = struct.unpack("!H", data[:2])
                    reason = data[2:].decode("utf-8", "replace")
                raise WSClosed(code, reason)
            if opcode == _OP_PING:
                self._send_control(_OP_PONG, data)
                continue
            if opcode == _OP_PONG:
                continue
            if opcode in (_OP_TEXT, _OP_BIN, _OP_CONT):
                message += data
                if fin:
                    return message.decode("utf-8", "replace")

    def _send_control(self, opcode: int, data: bytes = b"") -> None:
        mask = os.urandom(4)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
        self._sock.sendall(bytes([0x80 | opcode, 0x80 | len(data)]) + mask + masked)

    def close(self) -> None:
        try:
            self._send_control(_OP_CLOSE, struct.pack("!H", 1000))
        except OSError:
            pass
        try:
            self._sock.close()
        except OSError:
            pass
