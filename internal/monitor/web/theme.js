'use strict';
// Apply a validated preference before styles load; storage may be unavailable.
(() => {
  let theme = 'system';
  try { theme = localStorage.getItem('hrx-theme') || 'system'; } catch {}
  if (theme === 'light' || theme === 'dark') document.documentElement.dataset.theme = theme;
  document.addEventListener('DOMContentLoaded', () => {
    const select = document.getElementById('theme');
    select.value = document.documentElement.dataset.theme || 'system';
    select.addEventListener('change', () => {
      const value = select.value === 'dark' || select.value === 'light' ? select.value : 'system';
      if (value === 'system') delete document.documentElement.dataset.theme;
      else document.documentElement.dataset.theme = value;
      try { localStorage.setItem('hrx-theme', value); } catch {}
    });
  });
})();
