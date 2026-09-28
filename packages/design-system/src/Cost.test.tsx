import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { CostFigure, type CostFigureTotal } from "./Cost.tsx";

/** The figure's text as the cell shows it, and its data-cost-figure status. */
function figure(status: string, totals: CostFigureTotal[] | null | undefined): { text: string; status: string } {
  const html = renderToStaticMarkup(<CostFigure status={status} totals={totals} />);
  const m = /<span[^>]*data-cost-figure="([^"]*)"[^>]*>([^<]*)<\/span>/.exec(html);
  if (!m) throw new Error(`no data-cost-figure in ${html}`);
  return { status: m[1]!, text: m[2]!.replace(/&lt;/g, "<").replace(/&gt;/g, ">") };
}

const usd = (amount: string, estimate = "0"): CostFigureTotal => ({ currency: "USD", amount, estimate });

test("CostFigure: a total with an estimate part may still change and reads ~", () => {
  expect(figure("complete", [usd("1.4343", "1.28431")]).text).toBe("~$1.4343");
  expect(figure("final", [usd("0.5", "0.1")]).text).toBe("~$0.50");
});

test("CostFigure: incomplete reads ~ even with no estimate part", () => {
  expect(figure("incomplete", [usd("0.041")]).text).toBe("~$0.041");
});

test("CostFigure: a final total with no estimate part has no ~", () => {
  expect(figure("final", [usd("0.184215")]).text).toBe("$0.1842");
  expect(figure("complete", [usd("2")]).text).toBe("$2.00");
});

test("CostFigure: pending, or no totals, is an en dash", () => {
  expect(figure("pending", [])).toEqual({ status: "pending", text: "–" });
  expect(figure("pending", [usd("1")])).toEqual({ status: "pending", text: "–" });
  expect(figure("final", null)).toEqual({ status: "pending", text: "–" });
});

test("CostFigure: several currencies read multi, never a sum", () => {
  expect(figure("complete", [{ currency: "EUR", amount: "2.1", estimate: "2.1" }, usd("0.5")]).text).toBe("multi");
  expect(figure("final", [{ currency: "EUR", amount: "2.1", estimate: "0" }, usd("0.5")]).text).toBe("multi");
});
