import assert from "node:assert/strict";
import test from "node:test";

import { hashNetwork, networkGroup, saltNetwork, sourceNetwork } from "./network.js";

test("IPv4 groups exact addresses and maps IPv4-mapped IPv6 consistently", () => {
  assert.equal(networkGroup(" 203.0.113.9 "), "203.0.113.9");
  assert.equal(networkGroup("::ffff:203.0.113.9"), "203.0.113.9");
  assert.equal(networkGroup("0:0:0:0:0:FFFF:CB00:7109"), "203.0.113.9");
  assert.notEqual(networkGroup("203.0.113.9"), networkGroup("203.0.113.10"));
});

test("native IPv6 groups privacy addresses by /64", () => {
  assert.equal(networkGroup("2001:db8:abcd:1234::1"), networkGroup("2001:0DB8:ABCD:1234:ffff:ffff:ffff:ffff"));
  assert.notEqual(networkGroup("2001:db8:abcd:1234::1"), networkGroup("2001:db8:abcd:1235::1"));
  assert.notEqual(networkGroup("::203.0.113.9"), networkGroup("203.0.113.9"));
});

test("invalid addresses fail closed", () => {
  for (const value of [undefined, null, "", "hostname", "203.0.113.9, 198.51.100.1", "203.0.113.9:443", "256.1.1.1", "01.2.3.4", "1.2.3", "1.2.3.4.5", "[::1]", "fe80::1%eth0", "::1:443:bad:address", "1::2::3", "12345::", "http://127.0.0.1", "::ffff:256.1.1.1"])
    assert.equal(networkGroup(value), null, `Unexpected address admission: ${String(value)}`);
});

test("edge IP is mandatory and forged alternatives never select the network", () => {
  assert.equal(sourceNetwork(new Headers({ "X-Forwarded-For": "203.0.113.1", "X-Real-IP": "203.0.113.2" })), null);
  const headers = new Headers({ "CF-Connecting-IP": "203.0.113.9", "X-Forwarded-For": "198.51.100.1", "X-Real-IP": "198.51.100.2", "X-Bonjou-Network": "forged" });
  assert.equal(sourceNetwork(headers), "203.0.113.9");
  headers.append("CF-Connecting-IP", "198.51.100.3");
  assert.equal(sourceNetwork(headers), null);
});

test("Worker subrequests and expanded cross-zone sentinel fail closed", () => {
  for (const value of ["example.com", ""])
    assert.equal(sourceNetwork(new Headers({ "CF-Connecting-IP": "203.0.113.9", "CF-Worker": value })), null);
  for (const value of ["2a06:98c0:3600::103", "2A06:98C0:3600:0:0:0:0:0103"])
    assert.equal(sourceNetwork(new Headers({ "CF-Connecting-IP": value })), null);
});

test("private routing uses only a digest and preserves canonical grouping", async () => {
  const first = await hashNetwork(networkGroup("203.0.113.9"));
  assert.match(first, /^[a-f0-9]{64}$/);
  assert.equal(first, await hashNetwork(networkGroup("::ffff:203.0.113.9")));
  assert.notEqual(first, await hashNetwork(networkGroup("203.0.113.10")));
  assert.equal(await hashNetwork(networkGroup("2001:db8:1:2::1")), await hashNetwork(networkGroup("2001:db8:1:2::abc")));
});

test("persisted private routing salt prevents provisional address-digest linkage", async () => {
  const provisional = await hashNetwork(networkGroup("203.0.113.9"));
  const salt = new Uint8Array(32).fill(1);
  const first = await saltNetwork(provisional, salt);
  assert.match(first, /^[a-f0-9]{64}$/);
  assert.notEqual(first, provisional);
  assert.equal(first, await saltNetwork(provisional, new Uint8Array(salt)));
  assert.notEqual(first, await saltNetwork(provisional, new Uint8Array(32).fill(2)));
  assert.notEqual(first, await saltNetwork(await hashNetwork(networkGroup("203.0.113.10")), salt));
  await assert.rejects(saltNetwork("invalid", salt), TypeError);
  await assert.rejects(saltNetwork(provisional, new Uint8Array(31)), TypeError);
});
