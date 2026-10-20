"""A WBXML 1.3 codec for Exchange ActiveSync, written for the end-to-end test.

This is the client half of the test: it encodes requests the way [WBXML1.2]
and [MS-ASWBXML] say a client encodes them -- four byte header, the content
bit (0x40) set on every element that has content, an END token for each of
those, and code pages switched in the stream -- and it decodes responses the
same way.

Nothing here is derived from the server. The tag tables come from
aswbxml_tags.py, which is generated from the published specification, so a
response the server writes that this decoder cannot resolve is a response no
real ActiveSync client could read either.

The decoder is deliberately not a mirror image of the encoder: it accepts a
sloppy stream (a missing content bit, an END that closes nothing) and reports
what it saw as an anomaly instead of raising, because "the server omits the
content bit on MoreAvailable" is a finding this test exists to make.
"""

from __future__ import annotations

from aswbxml_tags import CODE_PAGES, TAGS

# --- Global tokens (WBXML1.2 §3.6) ---------------------------------------

SWITCH_PAGE = 0x00
END = 0x01
ENTITY = 0x02
STR_I = 0x03
LITERAL = 0x04
EXT_I_0, EXT_I_1, EXT_I_2 = 0x40, 0x41, 0x42
PI = 0x43
LITERAL_C = 0x44
STR_T = 0x83
OPAQUE = 0xC3

# A tag token carries bit 6 for "has content" and bit 7 for "has attributes".
HAS_CONTENT = 0x40
HAS_ATTRIBUTES = 0x80

# The header ActiveSync clients write: WBXML 1.3, unknown public identifier
# (0x01), UTF-8 (MIBEnum 106 = 0x6A), no string table.
HEADER = bytes([0x03, 0x01, 0x6A, 0x00])

PAGE_OF = {name: page for page, name in CODE_PAGES.items()}


class CodecError(Exception):
    """The stream could not be read at all."""


class Node:
    """One element: its namespace, name, text, opaque payload and children."""

    def __init__(self, ns: str, name: str):
        self.ns = ns
        self.name = name
        self.text = ""
        self.opaque: bytes = b""
        self.children: list[Node] = []

    # --- building -------------------------------------------------------

    def add(self, *kids) -> "Node":
        for kid in kids:
            if isinstance(kid, Node):
                self.children.append(kid)
            elif isinstance(kid, str):
                self.text += kid
            elif isinstance(kid, bytes):
                self.opaque += kid
            else:
                raise TypeError(f"cannot put {kid!r} in an element")
        return self

    # --- reading --------------------------------------------------------

    def find(self, name: str) -> "Node | None":
        for kid in self.children:
            if kid.name == name:
                return kid
        return None

    def findall(self, name: str) -> list["Node"]:
        return [kid for kid in self.children if kid.name == name]

    def descendant(self, name: str) -> "Node | None":
        for kid in self.children:
            if kid.name == name:
                return kid
            found = kid.descendant(name)
            if found is not None:
                return found
        return None

    def text_of(self, name: str) -> str:
        kid = self.find(name)
        return kid.text if kid else ""

    def text_at(self, name: str) -> str:
        kid = self.descendant(name)
        return kid.text if kid else ""

    def int_at(self, name: str, default: int = -1) -> int:
        raw = self.text_at(name).strip()
        try:
            return int(raw)
        except ValueError:
            return default

    def __repr__(self) -> str:
        return f"<{self.ns}:{self.name} {self.text!r} {len(self.children)} children>"


def el(ns: str, name: str, *kids) -> Node:
    """Build an element: `el("AirSync", "SyncKey", "0")`."""
    return Node(ns, name).add(*kids)


# --- Encoding -------------------------------------------------------------


def encode(root: Node) -> bytes:
    """Encode one root element as a WBXML document."""
    out = bytearray(HEADER)
    state = _EncoderState(out)
    state.element(root)
    return bytes(out)


def _string(text: str) -> bytes:
    if not text:
        return b""
    return bytes([STR_I]) + text.encode("utf-8") + b"\x00"


def _opaque(data: bytes) -> bytes:
    if not data:
        return b""
    return bytes([OPAQUE]) + _mbuint(len(data)) + data


def _mbuint(value: int) -> bytes:
    out = bytearray()
    while value >= 0x80:
        out.append((value & 0x7F) | 0x80)
        value >>= 7
    out.append(value)
    return bytes(out)


class _EncoderState:
    def __init__(self, out: bytearray):
        self.out = out
        self.page = 0

    def element(self, node: Node) -> None:
        node_page = PAGE_OF.get(node.ns)
        if node_page is None:
            raise CodecError(f"no code page for namespace {node.ns!r}")
        token = TAGS.get(node.ns, {}).get(node.name)
        if token is None:
            raise CodecError(f"no token for {node.ns}:{node.name}")
        # The code page is positional state in the stream, not a property of
        # the tree: an END token does not restore it, so a sibling after a
        # subtree that switched pages needs its own switch back. Tracking the
        # page in one place makes that automatic.
        if node_page != self.page:
            self.out.append(SWITCH_PAGE)
            self.out.append(node_page)
            self.page = node_page

        has_content = bool(node.children or node.text or node.opaque)
        self.out.append(token | HAS_CONTENT if has_content else token)
        if not has_content:
            return
        self.out += _string(node.text)
        self.out += _opaque(node.opaque)
        for kid in node.children:
            self.element(kid)
        self.out.append(END)


# --- Decoding -------------------------------------------------------------


class Report:
    """What the decoder saw that a strict client would have objected to."""

    def __init__(self) -> None:
        self.anomalies: list[str] = []

    def note(self, message: str) -> None:
        if message not in self.anomalies:
            self.anomalies.append(message)

    @property
    def clean(self) -> bool:
        return not self.anomalies


def decode(data: bytes) -> tuple[Node, Report]:
    """Decode a WBXML document into its root element and an anomaly report."""
    report = Report()
    pos = _read_header(data, report)

    root = Node("", "#document")
    stack = [root]
    page = 0

    while pos < len(data):
        b = data[pos]
        pos += 1

        if b == SWITCH_PAGE:
            if pos >= len(data):
                raise CodecError("SWITCH_PAGE with no page number")
            page = data[pos]
            pos += 1
            continue
        if b == END:
            if len(stack) > 1:
                stack.pop()
            else:
                report.note("END with no open element")
            continue
        if b == STR_I:
            end = data.find(b"\x00", pos)
            if end < 0:
                raise CodecError("unterminated inline string")
            stack[-1].text += data[pos:end].decode("utf-8", "replace")
            pos = end + 1
            continue
        if b == OPAQUE:
            length, pos = _read_mbuint(data, pos)
            if pos + length > len(data):
                raise CodecError("truncated opaque payload")
            stack[-1].opaque += data[pos:pos + length]
            pos += length
            continue
        if b in (ENTITY, STR_T, LITERAL, LITERAL_C, PI,
                 EXT_I_0, EXT_I_1, EXT_I_2) or b >= 0x84:
            raise CodecError(f"unsupported WBXML global token 0x{b:02X}")

        # A tag token: 0x05..0x3F without content, 0x45..0x7F with it. The
        # values 0x40..0x44 are global tokens and were handled above.
        if 0x05 <= b <= 0x7F:
            token = b & 0x3F if b >= HAS_CONTENT else b
            has_content = bool(b & HAS_CONTENT)
            name, known = _name(page, token)
            if not known:
                report.note(f"unknown tag 0x{token:02X} in code page {page}")
            node = Node(CODE_PAGES.get(page, f"?page{page}"), name)
            stack[-1].children.append(node)
            if has_content:
                stack.append(node)
            elif pos < len(data) and data[pos] in (STR_I, OPAQUE):
                # No content bit but content follows: a writer that forgot the
                # bit. Read it as content so the rest of the stream still
                # lines up, and say so.
                report.note(f"content bit missing on {node.ns}:{node.name}")
                stack.append(node)
            continue
        raise CodecError(f"unexpected byte 0x{b:02X}")

    if len(root.children) != 1:
        raise CodecError(f"expected one root element, found {len(root.children)}")
    return root.children[0], report


def _name(page: int, token: int) -> tuple[str, bool]:
    ns = CODE_PAGES.get(page)
    if ns is None:
        return f"?token0x{token:02X}", False
    for name, value in TAGS.get(ns, {}).items():
        if value == token:
            return name, True
    return f"?token0x{token:02X}", False


def _read_header(data: bytes, report: Report) -> int:
    if len(data) < 4:
        raise CodecError("stream too short for a WBXML header")
    if data[0] != 0x03:
        raise CodecError(f"unexpected WBXML version 0x{data[0]:02X}")
    pos = 1
    public_id, pos = _read_mbuint(data, pos)
    charset, pos = _read_mbuint(data, pos)
    table_len, pos = _read_mbuint(data, pos)
    if charset != 0x6A:
        report.note(f"charset {charset} is not UTF-8")
    if table_len:
        if pos + table_len > len(data):
            raise CodecError("truncated string table")
        report.note("the document carries a string table")
        pos += table_len
    return pos


def _read_mbuint(data: bytes, pos: int) -> tuple[int, int]:
    value = 0
    shift = 0
    while True:
        if pos >= len(data):
            raise CodecError("truncated multi-byte integer")
        b = data[pos]
        pos += 1
        value |= (b & 0x7F) << shift
        if not b & 0x80:
            return value, pos
        shift += 7
        if shift > 28:
            raise CodecError("multi-byte integer too long")
