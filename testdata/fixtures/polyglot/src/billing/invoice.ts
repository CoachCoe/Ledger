import { taxFor } from "./tax.js";

export interface Line {
  description: string;
  amount: number;
}

export class Invoice {
  constructor(readonly lines: Line[]) {}

  subtotal(): number {
    return this.lines.reduce((sum, line) => sum + line.amount, 0);
  }

  total(): number {
    return this.subtotal() + taxFor(this.subtotal());
  }
}
