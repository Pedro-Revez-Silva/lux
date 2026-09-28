import { expect, test } from "bun:test";
import { formatMoney, formatMoneyExact, moneyIsRounded } from "./format.ts";

// The same cases as TestMoney in internal/cli/costs_test.go: one rounding
// rule for the console and the CLI; only the currency's placement differs.
test("formatMoney rounds half to even to 4 decimals, with a bound below that", () => {
  const cases: [string, string][] = [
    ["1.28431", "$1.2843"],
    ["0.150000001", "$0.15"],
    ["2", "$2.00"],
    ["10.5", "$10.50"],
    ["0", "$0.00"],
    ["0.00005", "<$0.0001"],
    ["0.000074", "$0.0001"],
    ["0.00004", "<$0.0001"],
    ["0.00015", "$0.0002"],
    ["0.00025", "$0.0002"],
    ["0.000250001", "$0.0003"],
    ["1.99995", "$2.00"],
    ["12345.5", "$12,345.50"],
    ["-0.5", "-$0.50"],
    ["-0.00015", "-$0.0002"],
    ["-0.00001", ">-$0.0001"],
  ];
  for (const [amount, want] of cases) expect([amount, formatMoney(amount, "USD")]).toEqual([amount, want]);
});

test("formatMoney keeps unknown codes after the number and dashes the missing", () => {
  expect(formatMoney("3.2", "XTS")).toBe("3.20 XTS");
  expect(formatMoney("0.00001", "XTS")).toBe("<0.0001 XTS");
  expect(formatMoney("2.1", "EUR")).toBe("€2.10");
  expect(formatMoney(null, "USD")).toBe("–");
  expect(formatMoney("abc", "USD")).toBe("–");
  expect(formatMoney("1.5")).toBe("1.50");
});

test("fixed decimals override the rule", () => {
  expect(formatMoney("0.384", "USD", { decimals: 3 })).toBe("$0.384");
  expect(formatMoney("0.0000001", "USD", { decimals: 6 })).toBe("<$0.000001");
});

test("formatMoneyExact shows every digit; moneyIsRounded says when that differs", () => {
  expect(formatMoneyExact("0.000074", "USD")).toBe("$0.000074");
  expect(formatMoneyExact("2", "USD")).toBe("$2.00");
  expect(moneyIsRounded("0.000074")).toBe(true);
  expect(moneyIsRounded("1.2843")).toBe(false);
  expect(moneyIsRounded("2")).toBe(false);
  expect(moneyIsRounded("abc")).toBe(false);
});
