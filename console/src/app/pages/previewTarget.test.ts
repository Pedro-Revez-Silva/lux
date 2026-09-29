import { describe, expect, test } from "bun:test";
import { parsePreviewTarget, parsePreviewUrl, previewAuthUrl } from "./previewTarget.ts";

const SUFFIX = "k3jq7x2mfa2vbn4z";

describe("parsePreviewTarget", () => {
  test("a preview of this lux", () => {
    const t = parsePreviewTarget(`https://my-app-${SUFFIX}.lux.example.com/a?b=c#d`, "lux.example.com");
    if ("error" in t) throw new Error(t.error);
    expect(t.server).toBe("my-app");
    expect(t.runId).toBe(`run_${SUFFIX}`);
    expect(previewAuthUrl(t, "tkt_x")).toBe(`https://my-app-${SUFFIX}.lux.example.com/.lux/auth?ticket=tkt_x&to=%2Fa%3Fb%3Dc%23d`);
  });

  test("the domain's case and a trailing dot do not matter", () => {
    expect("error" in parsePreviewTarget(`https://web-${SUFFIX}.Lux.Example.com./`, "lux.example.com.")).toBe(false);
  });

  test.each([
    [`https://web-${SUFFIX}.evil.com/`, "another domain"],
    [`https://web-${SUFFIX}.lux.example.com.evil.com/`, "this domain inside another"],
    [`https://web-${SUFFIX}.evillux.example.com/`, "a suffix of a label"],
    [`https://a.web-${SUFFIX}.lux.example.com/`, "a deeper host"],
    [`https://web-${SUFFIX}.example.com/`, "the parent domain"],
    [`http://web-${SUFFIX}.lux.example.com/`, "http"],
    [`https://web-${SUFFIX}.lux.example.com:8443/`, "another port"],
    [`https://web.lux.example.com/`, "no run"],
    ["/relative", "not a URL"],
  ])("refuses %s (%s)", (to) => {
    expect("error" in parsePreviewTarget(to, "lux.example.com")).toBe(true);
  });

  test("previews off: nothing is a preview", () => {
    expect("error" in parsePreviewTarget(`https://web-${SUFFIX}.lux.example.com/`, null)).toBe(true);
  });

  test("the shape alone, for the sign-in screen, takes any domain", () => {
    expect("error" in parsePreviewUrl(`https://web-${SUFFIX}.evil.com/`)).toBe(false);
  });
});
