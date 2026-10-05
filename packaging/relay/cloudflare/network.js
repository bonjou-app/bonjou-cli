/** Source-network grouping for requests admitted by Cloudflare's edge. */

function address(value) {
  if (typeof value !== "string") return null;
  const raw = value.trim();
  if (/^(?:\d{1,3}\.){3}\d{1,3}$/.test(raw)) {
    const parts = raw.split(".");
    if (parts.some((part) => Number(part) > 255 || (part.length > 1 && part.startsWith("0")))) return null;
    const canonical = parts.map(Number).join(".");
    return { canonical, group: canonical };
  }
  // URL's standard IPv6 parser validates compression and embedded IPv4. The
  // character check excludes ports, scopes, bracket syntax, chains and URLs.
  if (!raw.includes(":") || !/^[\da-f:.]+$/i.test(raw)) return null;
  let canonical;
  try {
    canonical = new URL(`http://[${raw}]/`).hostname.slice(1, -1);
  } catch {
    return null;
  }
  const halves = canonical.split("::");
  const left = halves[0] ? halves[0].split(":") : [];
  const right = halves.length === 2 && halves[1] ? halves[1].split(":") : [];
  const words = halves.length === 2
    ? [...left, ...Array(8 - left.length - right.length).fill("0"), ...right].map((word) => Number.parseInt(word, 16))
    : left.map((word) => Number.parseInt(word, 16));
  if (words.length !== 8 || words.some((word) => !Number.isInteger(word) || word < 0 || word > 65535)) return null;
  if (words.slice(0, 5).every((word) => word === 0) && words[5] === 0xffff) {
    const ipv4 = [words[6] >> 8, words[6] & 255, words[7] >> 8, words[7] & 255].join(".");
    return { canonical: ipv4, group: ipv4 };
  }
  return { canonical, group: `${words.slice(0, 4).map((word) => word.toString(16)).join(":")}:0:0:0:0/64` };
}

export function networkGroup(value) {
  return address(value)?.group ?? null;
}

export function sourceNetwork(headers) {
  // Other Workers can transform client-IP headers; cross-zone Worker requests
  // also share one sentinel address. Neither is an ordinary browser network.
  if (headers.has("CF-Worker")) return null;
  const parsed = address(headers.get("CF-Connecting-IP"));
  if (!parsed || parsed.canonical === "2a06:98c0:3600::103") return null;
  // X-Forwarded-For, X-Real-IP and incoming private routing headers are ignored.
  return parsed.group;
}

export async function hashNetwork(group) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(`bonjou/coordinator/network/v1:${group}`));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

export async function saltNetwork(provisional, salt) {
  if (typeof provisional !== "string" || !/^[a-f0-9]{64}$/.test(provisional) || !(salt instanceof Uint8Array) || salt.length !== 32)
    throw new TypeError("invalid private network digest or routing salt");
  const input = new Uint8Array(64);
  input.set(salt);
  input.set(Uint8Array.from(provisional.match(/../g), (byte) => Number.parseInt(byte, 16)), 32);
  const digest = await crypto.subtle.digest("SHA-256", input);
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
