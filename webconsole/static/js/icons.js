/* icons.js — 内联 SVG 线性图标（16px 网格，stroke 继承 currentColor）。 */

const PATHS = {
  gauge: '<circle cx="8" cy="8.5" r="5.5"/><path d="M8 8.5 L10.8 5.7"/><path d="M2 14h12"/>',
  broadcast: '<circle cx="8" cy="8" r="1.6"/><path d="M4.8 11.2a4.5 4.5 0 0 1 0-6.4"/><path d="M11.2 4.8a4.5 4.5 0 0 1 0 6.4"/><path d="M2.7 13.3a7.5 7.5 0 0 1 0-10.6"/><path d="M13.3 2.7a7.5 7.5 0 0 1 0 10.6"/>',
  film: '<rect x="2" y="3" width="12" height="10" rx="1.5"/><path d="M5 3v10M11 3v10M2 6.5h3M2 9.5h3M11 6.5h3M11 9.5h3"/>',
  list: '<path d="M5.5 4h8M5.5 8h8M5.5 12h8"/><circle cx="3" cy="4" r=".9"/><circle cx="3" cy="8" r=".9"/><circle cx="3" cy="12" r=".9"/>',
  clock: '<circle cx="8" cy="8" r="6"/><path d="M8 4.5V8l2.4 1.6"/>',
  wand: '<path d="M3 13 10 6"/><path d="M11.5 2.5 12 4l1.5.5L12 5l-.5 1.5L11 5 9.5 4.5 11 4z"/><path d="M5.5 2v2M2 5.5h2"/>',
  bell: '<path d="M12 11H4c1-1 1.5-2 1.5-4.5a2.5 2.5 0 0 1 5 0C10.5 9 11 10 12 11z"/><path d="M6.8 13a1.3 1.3 0 0 0 2.4 0"/>',
  hook: '<path d="M8 2v7a3 3 0 1 0 3 3"/><path d="M6 2h4"/>',
  shield: '<path d="M8 2l5 2v4c0 3-2 5-5 6-3-1-5-3-5-6V4z"/><path d="M5.8 8l1.6 1.6L10.5 6.5"/>',
  chart: '<path d="M2 14h12"/><path d="M4 14V8M8 14V4M12 14V6"/>',
  route: '<circle cx="4" cy="4" r="1.8"/><circle cx="12" cy="12" r="1.8"/><path d="M4 6v3a3 3 0 0 0 3 3h3.2"/>',
  layers: '<path d="M8 2 14 5 8 8 2 5z"/><path d="M2 8.5 8 11.5 14 8.5"/><path d="M2 11.5 8 14.5 14 11.5"/>',
  camera: '<rect x="2" y="5" width="12" height="8" rx="1.5"/><path d="M5.5 5 7 3h2l1.5 2"/><circle cx="8" cy="9" r="2"/>',
  users: '<circle cx="6" cy="5.5" r="2.3"/><path d="M2 13c0-2.2 1.8-3.5 4-3.5s4 1.3 4 3.5"/><circle cx="11.5" cy="6" r="1.8"/><path d="M11 9.7c1.8.2 3 1.4 3 3.3"/>',
  cache: '<rect x="2.5" y="3" width="11" height="3.4" rx="1"/><rect x="2.5" y="9.6" width="11" height="3.4" rx="1"/><path d="M5 4.7h.01M5 11.3h.01"/>',
  server: '<rect x="2.5" y="2.5" width="11" height="4.6" rx="1.2"/><rect x="2.5" y="8.9" width="11" height="4.6" rx="1.2"/><path d="M5 4.8h.01M5 11.2h.01M12 4.8h.01M12 11.2h.01"/>',
  template: '<rect x="2.5" y="2.5" width="11" height="11" rx="1.5"/><path d="M2.5 6h11M6.5 6v7.5"/>',
  sparkle: '<path d="M8 2l1.4 3.6L13 7l-3.6 1.4L8 12 6.6 8.4 3 7l3.6-1.4z"/><path d="M12.5 10.5l.7 1.8 1.8.7-1.8.7-.7 1.8-.7-1.8-1.8-.7 1.8-.7z"/>',
};

export function icon(name) {
  const d = PATHS[name] || PATHS.gauge;
  return '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.3" ' +
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + d + '</svg>';
}
