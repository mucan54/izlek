#!/usr/bin/env python3
"""Reference encoder + test-vector generator for the LA1 wire format, clock
correction, murmur3_32, flag evaluation and the LAR1 replay format.

This file is the *independent reference implementation* the vectors are
produced from. Format changes are made here first (docs/design.md §17.1):

    python3 spec/vectors/gen.py          # rewrites spec/vectors/*.json
    python3 spec/vectors/gen.py --check  # fails if checked-in files differ
"""
import json
import math
import os
import re
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# ───────────────────────── primitives ─────────────────────────


def varint(n: int) -> bytes:
    assert 0 <= n < (1 << 64)
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def zigzag(n: int) -> bytes:
    assert -(1 << 63) <= n < (1 << 63)
    return varint(((n << 1) ^ (n >> 63)) & 0xFFFFFFFFFFFFFFFF)


def string(s: str) -> bytes:
    b = s.encode("utf-8")
    return varint(len(b)) + b


def parse_float(s: str) -> float:
    special = {"NaN": math.nan, "Infinity": math.inf, "-Infinity": -math.inf}
    return special[s] if s in special else float(s)


TAGS = {"null": 0, "false": 1, "true": 2, "int": 3, "float": 4, "string": 5, "json": 6}


def prop(p: dict) -> bytes:
    out = string(p["k"])
    t, v = p["t"], p["v"]
    if t == "null":
        return out + bytes([0])
    if t == "bool":
        return out + bytes([2 if v else 1])
    if t == "int":
        return out + bytes([3]) + zigzag(int(v))
    if t == "float":
        return out + bytes([4]) + struct.pack("<d", parse_float(v))
    if t == "string":
        return out + bytes([5]) + string(v)
    if t == "json":
        return out + bytes([6]) + string(v)
    raise ValueError(t)


def proplist(props: list) -> bytes:
    return varint(len(props)) + b"".join(prop(p) for p in props)


def header_body(b: dict) -> bytes:
    return (bytes.fromhex(b["batch_id"]) + varint(b["sent_at"]) + string(b["sdk"]) +
            string(b["anon_id"]) + string(b["user_id"]) + bytes.fromhex(b["session_id"]) +
            proplist(b["context"]))


def record_body(r: dict) -> bytes:
    return (bytes([r["kind"]]) + bytes.fromhex(r["uuid"]) + varint(r["ts"]) +
            string(r["name"]) + proplist(r["props"]))


def framed(body: bytes) -> bytes:
    return varint(len(body)) + body


MAGIC = b"LA1\x00"


def encode_batch(b: dict) -> bytes:
    return MAGIC + framed(header_body(b)) + b"".join(framed(record_body(r)) for r in b["records"])


# ───────────────────────── wire vectors ─────────────────────────

ZERO16 = "00" * 16


def P(k, t, v=None):
    return {"k": k, "t": t, "v": v}


def wire_vectors() -> dict:
    valid = []

    def add(name, batch):
        valid.append({"name": name, "batch": batch, "hex": encode_batch(batch).hex()})

    add("empty_batch", {
        "batch_id": "0190a5d0e4b17c3a8f00112233445566", "sent_at": 0, "sdk": "", "anon_id": "",
        "user_id": "", "session_id": ZERO16, "context": [], "records": []})

    add("single_event", {
        "batch_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4a5b", "sent_at": 1795046400123, "sdk": "kmp/0.1.0",
        "anon_id": "0193f1a2-0000-7000-8000-000000000001", "user_id": "",
        "session_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4a5c",
        "context": [P("platform", "string", "android"), P("os_version", "string", "15"),
                    P("screen_w", "int", "1080"), P("screen_h", "int", "2400")],
        "records": [{"kind": 0, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4a5d", "ts": 1795046399000,
                     "name": "app_opened", "props": []}]})

    add("all_value_types", {
        "batch_id": "ffffffffffffffffffffffffffffffff", "sent_at": 1, "sdk": "web/0.1.0",
        "anon_id": "a", "user_id": "u", "session_id": ZERO16,
        "context": [],
        "records": [{"kind": 0, "uuid": "000102030405060708090a0b0c0d0e0f", "ts": 127,
                     "name": "types", "props": [
                         P("n", "null"), P("f", "bool", False), P("t", "bool", True),
                         P("i0", "int", "0"), P("i1", "int", "1"), P("im1", "int", "-1"),
                         P("i63", "int", "63"), P("i64", "int", "64"), P("im64", "int", "-64"),
                         P("im65", "int", "-65"), P("i300", "int", "300"),
                         P("imax", "int", "9223372036854775807"),
                         P("imin", "int", "-9223372036854775808"),
                         P("d0", "float", "0.0"), P("dm0", "float", "-0.0"), P("d15", "float", "1.5"),
                         P("dpi", "float", "3.141592653589793"), P("dbig", "float", "1e308"),
                         P("dtiny", "float", "5e-324"), P("dinf", "float", "Infinity"),
                         P("dninf", "float", "-Infinity"),
                         P("s", "string", "merhaba dünya"), P("empty", "string", ""),
                         P("emoji", "string", "çğıöşü İ 🚀"),
                         P("arr", "json", "[1,2,{\"a\":null}]"), P("obj", "json", "{\"k\":\"v\"}"),
                     ]}]})

    add("identify_set_reset", {
        "batch_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4b00", "sent_at": 1795046500000, "sdk": "kmp/0.1.0",
        "anon_id": "anon-1", "user_id": "user-42", "session_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4b01",
        "context": [P("platform", "string", "ios"), P("app_version", "string", "3.2.0")],
        "records": [
            {"kind": 1, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4b02", "ts": 1795046499000, "name": "",
             "props": [P("user_id", "string", "user-42"), P("plan", "string", "pro")]},
            {"kind": 2, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4b03", "ts": 1795046499500, "name": "",
             "props": [P("age", "int", "34"), P("score", "float", "0.75"), P("beta", "bool", True)]},
            {"kind": 3, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4b04", "ts": 1795046499900, "name": "",
             "props": []},
        ]})

    add("multi_record_long_fields", {
        "batch_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4c00", "sent_at": 1795046600000, "sdk": "web/0.1.0",
        "anon_id": "0193f1a2-b3c4-7d5e-8a9b-0c1d2e3f4c01", "user_id": "",
        "session_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4c02",
        "context": [P("url", "string", "https://example.com/a?b=1"), P("path", "string", "/a"),
                    P("referrer", "string", "https://www.google.com/"),
                    P("ua", "string", "Mozilla/5.0 (X11; Linux x86_64)"),
                    P("viewport_w", "int", "1280"), P("viewport_h", "int", "720")],
        "records": [
            {"kind": 0, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4c03", "ts": 1795046599000,
             "name": "pageview", "props": [P("title", "string", "x" * 300)]},
            {"kind": 0, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4c04", "ts": 1795046599500,
             "name": "purchase", "props": [P("amount", "float", "12.5"), P("currency", "string", "TRY"),
                                           P("items", "json", "[" + ",".join(["1"] * 200) + "]"),
                                           P("amount", "float", "13.0")]},
            {"kind": 0, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4c05", "ts": 1795046599999,
             "name": "pageleave", "props": [P("path", "string", "/b")]},
        ]})

    # NaN is kept separate: only the canonical quiet NaN (0x7ff8000000000000) is covered.
    add("float_nan", {
        "batch_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4d00", "sent_at": 5, "sdk": "x", "anon_id": "a",
        "user_id": "", "session_id": ZERO16, "context": [],
        "records": [{"kind": 0, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4d01", "ts": 5, "name": "nan",
                     "props": [P("v", "float", "NaN")]}]})

    # ── tolerant decoding: hex decodes to `batch` but re-encoding differs ──
    tolerant = []
    base = {"batch_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4e00", "sent_at": 9, "sdk": "x", "anon_id": "a",
            "user_id": "", "session_id": ZERO16, "context": [P("c", "int", "1")], "records": []}
    rec = {"kind": 0, "uuid": "0193f1a2b3c47d5e8a9b0c1d2e3f4e01", "ts": 9, "name": "e",
           "props": [P("k", "string", "v")]}

    hb = header_body(base) + b"\xde\xad"  # unknown trailing header bytes
    rb = record_body(rec) + b"\xbe\xef\x00"  # unknown trailing record bytes
    tolerant.append({"name": "trailing_bytes_ignored", "hex": (MAGIC + framed(hb) + framed(rb)).hex(),
                     "batch": {**base, "records": [rec]}})

    unk = dict(rec, kind=9, uuid="0193f1a2b3c47d5e8a9b0c1d2e3f4e02")
    tolerant.append({"name": "unknown_kind_skipped",
                     "hex": (MAGIC + framed(header_body(base)) + framed(record_body(unk)) +
                             framed(record_body(rec))).hex(),
                     "batch": {**base, "records": [rec]}})

    nonmin = MAGIC + bytes([0x80 | len(header_body(base)), 0x00]) + header_body(base)
    tolerant.append({"name": "non_minimal_varint_accepted", "hex": nonmin.hex(),
                     "batch": {**base, "records": []}})

    dup = dict(rec, props=[P("k", "string", "first"), P("k", "string", "last")])
    tolerant.append({"name": "duplicate_key_last_wins",
                     "hex": (MAGIC + framed(header_body(base)) + framed(record_body(dup))).hex(),
                     "batch": {**base, "records": [dict(rec, props=[P("k", "string", "last")])]},
                     "note": "decoders that return a map keep the last value; list decoders may keep both"})

    # ── invalid ──
    invalid = []
    good_h = framed(header_body(base))
    good_r = framed(record_body(rec))

    def bad(name, raw, frame_ok, header_ok=None, bad_records=None, status=None):
        invalid.append({"name": name, "hex": raw.hex(), "frame_ok": frame_ok, "header_ok": header_ok,
                        "bad_records": bad_records, "http_status": status})

    bad("empty_body", b"", False, status=400)
    bad("short_magic", b"LA1", False, status=400)
    bad("bad_magic", b"LA2\x00" + good_h, False, status=400)
    bad("missing_header", MAGIC, False, status=400)
    bad("header_len_too_small", MAGIC + framed(b"\x00" * 15), False, status=400)
    bad("header_truncated", MAGIC + good_h[:-3], False, status=400)
    bad("header_len_overflows_body", MAGIC + varint(1000) + header_body(base), False, status=400)
    bad("header_too_large", MAGIC + framed(header_body(base) + b"\x00" * 65536), False, status=400)
    bad("record_len_zero", MAGIC + good_h + b"\x00", False, status=400)
    bad("record_truncated", MAGIC + good_h + good_r[:-1], False, status=400)
    bad("trailing_garbage", MAGIC + good_h + good_r + b"\x05", False, status=400)
    bad("record_too_large", MAGIC + good_h + framed(record_body(rec) + b"\x00" * 65536), False, status=400)
    bad("varint_too_long", MAGIC + b"\x80" * 10 + b"\x01" + header_body(base), False, status=400)
    bad("varint_unterminated", MAGIC + good_h + b"\x80", False, status=400)
    bad("too_many_records", MAGIC + good_h + good_r * 501, False, status=413)

    # frame is fine, content is not → ingestion accepts (200), shipper rejects
    short_header = bytes.fromhex(base["batch_id"]) + varint(9) + b"\x05ab"  # sdk string overruns
    bad("header_string_overrun", MAGIC + framed(short_header), True, header_ok=False, status=200)
    bad_tag = record_body(dict(rec, props=[]))[:-1] + varint(1) + string("k") + bytes([7])
    bad("unknown_tag", MAGIC + good_h + framed(bad_tag) + good_r, True, header_ok=True,
        bad_records=[0], status=200)
    bad_utf8 = bytes([0]) + bytes.fromhex(rec["uuid"]) + varint(9) + b"\x02\xff\xfe" + varint(0)
    bad("invalid_utf8_name", MAGIC + good_h + good_r + framed(bad_utf8), True, header_ok=True,
        bad_records=[1], status=200)
    cut = record_body(rec)[:20]
    bad("record_body_short", MAGIC + good_h + framed(cut), True, header_ok=True, bad_records=[0],
        status=200)
    prop_overrun = record_body(dict(rec, props=[]))[:-1] + varint(3) + string("k") + bytes([0])
    bad("prop_count_overrun", MAGIC + good_h + framed(prop_overrun), True, header_ok=True,
        bad_records=[0], status=200)

    return {"format": "LA1", "valid": valid, "tolerant": tolerant, "invalid": invalid}


# ───────────────────────── clock correction ─────────────────────────

DAY = 86_400_000
CLOCK_CORRECTED, LATE = 1, 2


def clock(recv_ts: int, sent_at: int, ts: int):
    skew = 0 if sent_at == 0 else recv_ts - sent_at
    flags = 0
    if abs(skew) > 5000:
        ts += skew
        flags |= CLOCK_CORRECTED
    if ts > recv_ts:
        ts = recv_ts
        flags |= CLOCK_CORRECTED
    if recv_ts - ts > 30 * DAY:
        flags |= LATE
    if ts < 0:
        ts = 0
    return ts, flags


def clock_vectors() -> dict:
    R = 1795046400000
    cases = [
        ("in_sync", R, R - 40, R - 1000),
        ("skew_just_under", R, R - 5000, R - 6000),
        ("skew_just_over", R, R - 5001, R - 6000),
        ("device_ahead_1h", R, R + 3_600_000, R + 3_590_000),
        ("device_behind_2d", R, R - 2 * DAY, R - 2 * DAY - 500),
        ("future_ts_no_skew", R, R - 10, R + 4000),
        ("future_after_correction", R, R - 6000, R - 1000),
        ("queued_6_days", R, R - 20, R - 6 * DAY),
        ("late_31_days", R, R - 20, R - 31 * DAY),
        ("exactly_30_days", R, R - 20, R - 30 * DAY),
        ("sent_at_unknown", R, 0, R - 1000),
        ("sent_at_unknown_future", R, 0, R + 1),
        ("epoch_device", R, 86_400_000, 86_399_000),
        ("negative_after_correction", 10_000, 100_000, 50_000),
    ]
    out = []
    for name, recv, sent, ts in cases:
        o, f = clock(recv, sent, ts)
        out.append({"name": name, "recv_ts": recv, "sent_at": sent, "ts": ts, "out_ts": o, "flags": f})
    return {"flag_bits": {"clock_corrected": 1, "late": 2}, "cases": out}


# ───────────────────────── murmur3 ─────────────────────────


def murmur3_32(data: bytes, seed: int = 0) -> int:
    c1, c2 = 0xCC9E2D51, 0x1B873593
    h = seed & 0xFFFFFFFF
    n = len(data)
    for i in range(0, n - n % 4, 4):
        k = struct.unpack_from("<I", data, i)[0]
        k = (k * c1) & 0xFFFFFFFF
        k = ((k << 15) | (k >> 17)) & 0xFFFFFFFF
        k = (k * c2) & 0xFFFFFFFF
        h ^= k
        h = ((h << 13) | (h >> 19)) & 0xFFFFFFFF
        h = (h * 5 + 0xE6546B64) & 0xFFFFFFFF
    tail = data[n - n % 4:]
    k = 0
    if len(tail) >= 3:
        k ^= tail[2] << 16
    if len(tail) >= 2:
        k ^= tail[1] << 8
    if len(tail) >= 1:
        k ^= tail[0]
        k = (k * c1) & 0xFFFFFFFF
        k = ((k << 15) | (k >> 17)) & 0xFFFFFFFF
        k = (k * c2) & 0xFFFFFFFF
        h ^= k
    h ^= n
    h ^= h >> 16
    h = (h * 0x85EBCA6B) & 0xFFFFFFFF
    h ^= h >> 13
    h = (h * 0xC2B2AE35) & 0xFFFFFFFF
    h ^= h >> 16
    return h


# Published reference values (seed 0) — guards the reference itself.
assert murmur3_32(b"") == 0
assert murmur3_32(b"hello") == 0x248BFA47
assert murmur3_32(b"The quick brown fox jumps over the lazy dog") == 0x2E4FF723


def murmur_vectors() -> dict:
    inputs = ["", "a", "ab", "abc", "abcd", "abcde", "hello", "Hello, world!",
              "The quick brown fox jumps over the lazy dog", "k3f9:new_checkout:user-42",
              "k3f9:v:new_checkout:user-42", "çğıöşü", "🚀🚀", "0", "1017", "x" * 255,
              "salt:flag:0193f1a2-b3c4-7d5e-8a9b-0c1d2e3f4c01"]
    return {"seed": 0, "cases": [{"input": s, "hash": murmur3_32(s.encode()),
                                  "bucket": murmur3_32(s.encode()) % 10000} for s in inputs]}


# ───────────────────────── flag evaluation ─────────────────────────

NUMERIC = re.compile(r"^-?[0-9]+(\.[0-9]+)?$")


def is_num(x):
    return isinstance(x, (int, float)) and not isinstance(x, bool)


def to_str(x) -> str:
    if isinstance(x, str):
        return x
    if isinstance(x, bool):
        return "true" if x else "false"
    if is_num(x):
        if float(x) == int(x):
            return str(int(x))
        return repr(float(x))
    return json.dumps(x, separators=(",", ":"), ensure_ascii=False)


def to_num(x):
    if is_num(x):
        return float(x)
    if isinstance(x, str) and NUMERIC.match(x):
        return float(x)
    return None


def loose_eq(p, v) -> bool:
    if is_num(p) and is_num(v):
        return float(p) == float(v)
    return to_str(p) == to_str(v)


def semver(x):
    s = to_str(x).strip()
    if s[:1] in ("v", "V"):
        s = s[1:]
    s = re.split(r"[-+]", s, maxsplit=1)[0]
    parts = s.split(".")
    if not 1 <= len(parts) <= 3 or not all(re.fullmatch(r"[0-9]+", p) for p in parts):
        return None
    nums = [int(p) for p in parts]
    return tuple(nums + [0] * (3 - len(nums)))


def match(cond, props) -> bool:
    if "and" in cond:
        return all(match(c, props) for c in cond["and"])
    if "or" in cond:
        return any(match(c, props) for c in cond["or"])
    if "not" in cond:
        return not match(cond["not"], props)
    p = props.get(cond["prop"])
    op, v = cond["op"], cond.get("value")
    if p is None:
        return op in ("ne", "not_in")
    if op == "is_set":
        return True
    if op == "eq":
        return loose_eq(p, v)
    if op == "ne":
        return not loose_eq(p, v)
    if op == "in":
        return isinstance(v, list) and any(loose_eq(p, x) for x in v)
    if op == "not_in":
        return not (isinstance(v, list) and any(loose_eq(p, x) for x in v))
    if op in ("gt", "gte", "lt", "lte"):
        a, b = to_num(p), to_num(v)
        if a is None or b is None:
            return False
        return {"gt": a > b, "gte": a >= b, "lt": a < b, "lte": a <= b}[op]
    if op == "contains":
        return to_str(v) in to_str(p)
    if op == "regex":
        try:
            return re.search(to_str(v), to_str(p)) is not None
        except re.error:
            return False
    if op in ("semver_gte", "semver_lt"):
        a, b = semver(p), semver(v)
        if a is None or b is None:
            return False
        return a >= b if op == "semver_gte" else a < b
    return False


def norm_default(d):
    return None if d is False else d


def eval_flag(flag, props, user_id, anon_id):
    props = dict(props, user_id=user_id or None, anon_id=anon_id)
    if not flag.get("enabled"):
        return norm_default(flag.get("default"))
    for rule in flag.get("rules", []):
        if rule.get("when") is not None and not match(rule["when"], props):
            continue
        ident = anon_id if rule.get("stable_on") == "anon_id" else (user_id or anon_id)
        rollout = rule.get("rollout", 10000)
        b = murmur3_32(f"{flag['salt']}:{flag['key']}:{ident}".encode()) % 10000
        if b >= rollout:
            return norm_default(flag.get("default"))
        variants = rule.get("variants") or []
        if not variants:
            return True
        v = murmur3_32(f"{flag['salt']}:v:{flag['key']}:{ident}".encode()) % 10000
        acc = 0
        for var in variants:
            acc += var["w"]
            if v < acc:
                return var["key"]
        return variants[-1]["key"]
    return norm_default(flag.get("default"))


def flag_vectors() -> dict:
    ruleset = {"v": 31, "flags": [
        {"key": "new_checkout", "enabled": True, "salt": "k3f9", "rules": [
            {"when": {"and": [{"prop": "platform", "op": "eq", "value": "ios"},
                              {"prop": "app_version", "op": "semver_gte", "value": "3.2.0"}]},
             "rollout": 2500,
             "variants": [{"key": "control", "w": 5000}, {"key": "b", "w": 5000}]}],
         "default": None},
        {"key": "everyone", "enabled": True, "salt": "s1", "rules": [{"rollout": 10000}], "default": None},
        {"key": "nobody", "enabled": True, "salt": "s2", "rules": [{"rollout": 0}], "default": None},
        {"key": "disabled_with_default", "enabled": False, "salt": "s3",
         "rules": [{"rollout": 10000}], "default": "legacy"},
        {"key": "half", "enabled": True, "salt": "h", "rules": [{"rollout": 5000}], "default": False},
        {"key": "sticky", "enabled": True, "salt": "st", "rules": [
            {"rollout": 5000, "stable_on": "anon_id",
             "variants": [{"key": "a", "w": 3333}, {"key": "b", "w": 3333}, {"key": "c", "w": 3334}]}],
         "default": None},
        {"key": "first_match_decides", "enabled": True, "salt": "fm", "rules": [
            {"when": {"prop": "plan", "op": "eq", "value": "pro"}, "rollout": 0},
            {"rollout": 10000}], "default": "fallback"},
        {"key": "ops", "enabled": True, "salt": "o", "rules": [
            {"when": {"or": [
                {"prop": "age", "op": "gte", "value": 18},
                {"prop": "country", "op": "in", "value": ["TR", "DE"]},
                {"prop": "email", "op": "regex", "value": "@example\\.com$"},
                {"not": {"prop": "plan", "op": "is_set"}}]},
             "rollout": 10000, "variants": [{"key": "x", "w": 10000}]}], "default": None},
        {"key": "ops2", "enabled": True, "salt": "o2", "rules": [
            {"when": {"and": [
                {"prop": "name", "op": "contains", "value": "ali"},
                {"prop": "tier", "op": "not_in", "value": [1, 2]},
                {"prop": "score", "op": "lt", "value": "10.5"},
                {"prop": "build", "op": "ne", "value": 7},
                {"prop": "app_version", "op": "semver_lt", "value": "v4"}]},
             "rollout": 10000}], "default": None},
        {"key": "short_weights", "enabled": True, "salt": "sw", "rules": [
            {"rollout": 10000, "variants": [{"key": "only", "w": 1}]}], "default": None},
        {"key": "empty_and", "enabled": True, "salt": "ea", "rules": [
            {"when": {"and": []}, "rollout": 10000}], "default": None},
        {"key": "empty_or", "enabled": True, "salt": "eo", "rules": [
            {"when": {"or": []}, "rollout": 10000}], "default": "none-matched"},
    ]}
    by_key = {f["key"]: f for f in ruleset["flags"]}
    cases = []

    def case(flag, props, user_id, anon_id):
        cases.append({"flag": flag, "props": props, "user_id": user_id, "anon_id": anon_id,
                      "expect": eval_flag(by_key[flag], props, user_id, anon_id)})

    ios = {"platform": "ios", "app_version": "3.2.0"}
    for i in range(24):
        case("new_checkout", ios, f"user-{i}", f"anon-{i}")
    case("new_checkout", {"platform": "android", "app_version": "3.2.0"}, "user-1", "anon-1")
    case("new_checkout", {"platform": "ios", "app_version": "3.1.9"}, "user-1", "anon-1")
    case("new_checkout", {"platform": "ios", "app_version": "3.10"}, "user-3", "anon-3")
    case("new_checkout", {"platform": "ios", "app_version": "v3.2.0-beta.1+7"}, "user-3", "anon-3")
    case("new_checkout", {"platform": "ios", "app_version": "garbage"}, "user-3", "anon-3")
    case("new_checkout", {"platform": "ios"}, "user-3", "anon-3")
    for f in ("everyone", "nobody", "disabled_with_default", "empty_and", "empty_or", "short_weights"):
        case(f, {}, "", "anon-9")
    for i in range(12):
        case("half", {}, "", f"anon-{i}")
        case("half", {}, f"user-{i}", f"anon-{i}")
    for i in range(12):
        case("sticky", {}, "", f"anon-{i}")
        case("sticky", {}, "someone-else", f"anon-{i}")  # must equal the anonymous result
    case("first_match_decides", {"plan": "pro"}, "u", "a")
    case("first_match_decides", {"plan": "free"}, "u", "a")
    case("first_match_decides", {}, "u", "a")
    for props in ({"age": 18, "plan": "x"}, {"age": "17", "plan": "x"}, {"age": "18.0", "plan": "x"},
                  {"age": "abc", "plan": "x"}, {"country": "TR", "plan": "x"},
                  {"country": "US", "plan": "x"}, {"email": "a@example.com", "plan": "x"},
                  {"email": "a@example.com.tr", "plan": "x"}, {"plan": "x"}, {},
                  {"plan": None}, {"age": True, "plan": "x"}):
        case("ops", props, "u", "a")
    good = {"name": "khalid alim", "tier": 3, "score": 10, "build": 8, "app_version": "3.9.9"}
    case("ops2", good, "u", "a")
    for k, v in (("name", "Ali"), ("tier", 2), ("tier", "2"), ("tier", 2.0), ("score", 10.5),
                 ("score", "9.99"), ("score", "1e1"), ("build", 7), ("build", "7"), ("build", 7.5),
                 ("app_version", "4.0.0"), ("app_version", "3")):
        case("ops2", dict(good, **{k: v}), "u", "a")
    for k in ("name", "tier", "score", "build", "app_version"):
        case("ops2", {x: y for x, y in good.items() if x != k}, "u", "a")

    seen = {json.dumps(c["expect"]) for c in cases if c["flag"] == "new_checkout"}
    assert {'null', '"control"', '"b"'} <= seen, seen
    assert {json.dumps(c["expect"]) for c in cases if c["flag"] == "half"} == {"null", "true"}
    assert len({json.dumps(c["expect"]) for c in cases if c["flag"] == "sticky"}) >= 3
    return {"ruleset": ruleset, "cases": cases}


# ───────────────────────── replay ─────────────────────────


def node(n: dict) -> bytes:
    flags = n["flags"]
    assert bool(flags & 1) == ("color" in n) and bool(flags & 2) == ("text" in n)
    out = (varint(n["id"]) + varint(n["parent"]) + bytes([n["type"]]) + zigzag(n["x"]) + zigzag(n["y"]) +
           varint(n["w"]) + varint(n["h"]) + bytes([flags]))
    if flags & 1:
        out += struct.pack("<I", n["color"])
    if flags & 2:
        out += string(n["text"])
    return out


def frame(f: dict) -> bytes:
    k = f["kind"]
    if k == 0:
        payload = varint(f["w"]) + varint(f["h"]) + varint(len(f["nodes"])) + b"".join(map(node, f["nodes"]))
    elif k == 1:
        payload = (varint(len(f["removed"])) + b"".join(map(varint, f["removed"])) +
                   varint(len(f["upserted"])) + b"".join(map(node, f["upserted"])))
    elif k == 2:
        payload = bytes([f["phase"]]) + varint(f["x"]) + varint(f["y"])
    elif k == 3:
        payload = varint(f["node_id"]) + zigzag(f["dx"]) + zigzag(f["dy"])
    elif k == 4:
        payload = string(f["name"])
    elif k == 5:
        payload = varint(f["w"]) + varint(f["h"])
    else:
        raise ValueError(k)
    return framed(bytes([k]) + varint(f["dt"]) + payload)


def encode_chunk(c: dict) -> bytes:
    return (b"LAR1" + bytes.fromhex(c["session_id"]) + varint(c["seq"]) + varint(c["start_ts"]) +
            b"".join(map(frame, c["frames"])))


def replay_vectors() -> dict:
    nodes = [
        {"id": 1, "parent": 0, "type": 0, "x": 0, "y": 0, "w": 390, "h": 844, "flags": 1, "color": 0xFFFFFFFF},
        {"id": 2, "parent": 1, "type": 1, "x": 16, "y": 60, "w": 200, "h": 24, "flags": 2, "text": "Hoş geldin"},
        {"id": 3, "parent": 1, "type": 4, "x": 16, "y": 100, "w": 358, "h": 44, "flags": 2 | 4 | 16,
         "text": "••••••"},
        {"id": 4, "parent": 1, "type": 2, "x": 16, "y": 160, "w": 120, "h": 120, "flags": 0},
        {"id": 5, "parent": 1, "type": 3, "x": 16, "y": 300, "w": 358, "h": 48, "flags": 1 | 2 | 8,
         "color": 0xFF0A84FF, "text": "Devam"},
        {"id": 6, "parent": 1, "type": 5, "x": -20, "y": -300, "w": 430, "h": 2000, "flags": 0},
    ]
    chunk = {"session_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4a5c", "seq": 0, "start_ts": 1795046400000,
             "frames": [
                 {"kind": 4, "dt": 0, "name": "Login"},
                 {"kind": 5, "dt": 0, "w": 390, "h": 844},
                 {"kind": 0, "dt": 0, "w": 390, "h": 844, "nodes": nodes},
                 {"kind": 2, "dt": 740, "phase": 0, "x": 195, "y": 324},
                 {"kind": 2, "dt": 16, "phase": 1, "x": 196, "y": 320},
                 {"kind": 2, "dt": 60, "phase": 2, "x": 196, "y": 320},
                 {"kind": 3, "dt": 500, "node_id": 6, "dx": 0, "dy": -128},
                 {"kind": 1, "dt": 500, "removed": [4],
                  "upserted": [{"id": 2, "parent": 1, "type": 1, "x": 16, "y": 60, "w": 220, "h": 24,
                                "flags": 2, "text": "Hoş geldin, Ayşe"},
                               {"id": 7, "parent": 1, "type": 6, "x": 0, "y": 800, "w": 390, "h": 44,
                                "flags": 0}]},
             ]}
    empty = {"session_id": "0193f1a2b3c47d5e8a9b0c1d2e3f4a5c", "seq": 300, "start_ts": 1795046405000,
             "frames": []}
    unknown_kind = encode_chunk(empty) + framed(bytes([99]) + varint(5) + b"\x01\x02\x03") + frame(
        {"kind": 5, "dt": 7, "w": 1, "h": 2})
    return {"format": "LAR1",
            "valid": [{"name": "login_screen", "chunk": chunk, "hex": encode_chunk(chunk).hex()},
                      {"name": "empty_chunk", "chunk": empty, "hex": encode_chunk(empty).hex()}],
            "tolerant": [{"name": "unknown_frame_kind_skipped", "hex": unknown_kind.hex(),
                          "chunk": dict(empty, frames=[{"kind": 5, "dt": 7, "w": 1, "h": 2}]),
                          "note": "dt of a skipped frame is still consumed by players (7ms after +5ms)"}]}


# ───────────────────────── main ─────────────────────────

FILES = {"wire.json": wire_vectors, "clock.json": clock_vectors, "murmur3.json": murmur_vectors,
         "flags.json": flag_vectors, "replay.json": replay_vectors}


def main():
    check = "--check" in sys.argv
    ok = True
    for name, fn in FILES.items():
        text = json.dumps(fn(), ensure_ascii=False, indent=1) + "\n"
        path = os.path.join(HERE, name)
        if check:
            if not os.path.exists(path) or open(path, encoding="utf-8").read() != text:
                print(f"STALE: {name}")
                ok = False
        else:
            with open(path, "w", encoding="utf-8") as f:
                f.write(text)
            print(f"wrote {name} ({len(text)} bytes)")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
