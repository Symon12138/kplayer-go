/* icons.js — 内联 SVG 线性图标（16px 网格，stroke 继承 currentColor）。 */

const PATHS = {
  gauge: '<circle cx="8" cy="8.5" r="5.5"/><path d="M8 8.5 L10.8 5.7"/><path d="M2 14h12"/>',
  broadcast: '<circle cx="8" cy="8" r="1.6"/><path d="M4.8 11.2a4.5 4.5 0 0 1 0-6.4"/><path d="M11.2 4.8a4.5 4.5 0 0 1 0 6.4"/><path d="M2.7 13.3a7.5 7.5 0 0 1 0-10.6"/><path d="M13.3 2.7a7.5 7.5 0 0 1 0 10.6"/>',
  film: '<rect x="2" y="3" width="12" height="10" rx="1.5"/><path d="M5 3v10M11 3v10M2 6.5h3M2 9.5h3M11 6.5h3M11 9.5h3"/>',
  list: '<path d="M5.5 4h8M5.5 8h8M5.5 12h8"/><circle cx="3" cy="4" r=".9"/><circle cx="3" cy="8" r=".9"/><circle cx="3" cy="12" r=".9"/>',
  clock: '<circle cx="8" cy="8" r="6"/><path d="M8 4.5V8l2.4 1.6"/>',
  wand: '<path d="M3 13 10 6"/><path d="M11.5 2.5 12 4l1.5.5L12 5l-.5 1.5L11 5 9.5 4.5 11 4z"/><path d="M5.5 2v2M2 5.5h2"/>',
};

export function icon(name) {
  const d = PATHS[name] || PATHS.gauge;
  return '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.3" ' +
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + d + '</svg>';
}
