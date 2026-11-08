export const RATE = 0.2;

export function taxFor(amount) {
  return Math.round(amount * RATE);
}
