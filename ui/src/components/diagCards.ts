// The Diagnostics cards a verdict's next step can lead to, by the step's
// id: the anchor each card carries, so the verdict can bring it forward.
export const DIAG_CARD = { bloat: "diag-bloat", trace: "diag-trace" } as const;

export type CardStep = keyof typeof DIAG_CARD;

export const isCardStep = (id: string): id is CardStep => id in DIAG_CARD;

// goToCard brings the step's card into view and puts the keyboard on its
// primary action (the button marked data-primary: Run test, Trace) when it
// can be pressed, so Enter runs it. It only leads there: the user still
// starts the test, which loads their line. While a test runs there is no
// such button, and the card is only shown.
export function goToCard(step: CardStep) {
  const card = document.getElementById(DIAG_CARD[step]);
  if (!card) return;
  const smooth = !window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  card.scrollIntoView({ behavior: smooth ? "smooth" : "auto", block: "start" });
  card.querySelector<HTMLButtonElement>("button[data-primary]:not(:disabled)")?.focus({ preventScroll: true });
}
