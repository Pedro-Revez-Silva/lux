import { describe, expect, test } from "bun:test";
import { parsePreviewTarget, parsePreviewUrl, previewAuthUrl } from "./previewTarget.ts";

describe("parsePreviewTarget", () => {
  test("a preview of this lux: one label or several under the domain", () => {
    const t = parsePreviewTarget("https://my-app-k3x9ab2c.lux.example.com/a?b=c#d", "lux.example.com");
    if ("error" in t) throw new Error(t.error);
    expect(t.hostname).toBe("my-app-k3x9ab2c.lux.example.com");
    expect(previewAuthUrl(t, "tkt_x")).toBe("https://my-app-k3x9ab2c.lux.example.com/.lux/auth?ticket=tkt_x&to=%2Fa%3Fb%3Dc%23d");
    const deep = parsePreviewTarget("https://web.t123.p9.lux.example.com/", "lux.example.com");
    expect("error" in deep ? deep.error : deep.hostname).toBe("web.t123.p9.lux.example.com");
  });

  test("the domain's case and a trailing dot do not matter", () => {
    expect("error" in parsePreviewTarget("https://web-k3x9ab2c.Lux.Example.com./", "lux.example.com.")).toBe(false);
  });

  test.each([
    ["https://web.evil.com/", "another domain"],
    ["https://web.lux.example.com.evil.com/", "this domain inside another"],
    ["https://web.evillux.example.com/", "a suffix of a label"],
    ["https://lux.example.com/", "the domain itself"],
    ["https://web.example.com/", "the parent domain"],
    ["http://web.lux.example.com/", "http"],
    ["https://web.lux.example.com:8443/", "another port"],
    ["https://we_b.lux.example.com/", "not a DNS label"],
    ["/relative", "not a URL"],
  ])("refuses %s (%s)", (to) => {
    expect("error" in parsePreviewTarget(to, "lux.example.com")).toBe(true);
  });

  test("a local demo: http on its port, under localhost", () => {
    const local = { previewDomain: "lux.localhost", previewScheme: "http" as const, previewPort: 8090 };
    expect("error" in parsePreviewTarget("http://web.pr1.lux.localhost:8090/", local)).toBe(false);
    expect("error" in parsePreviewTarget("http://web.pr1.lux.localhost/", local)).toBe(true);
    expect("error" in parsePreviewTarget("https://web.pr1.lux.localhost:8090/", local)).toBe(true);
  });

  test("previews off: nothing is a preview", () => {
    expect("error" in parsePreviewTarget("https://web.lux.example.com/", null)).toBe(true);
  });

  test("the shape alone, for the sign-in screen, takes any domain", () => {
    expect("error" in parsePreviewUrl("https://web.evil.com/")).toBe(false);
  });
});
