import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('../internal/carpool/web/assets/app.js', import.meta.url), 'utf8');
function application(session) {
  const context = vm.createContext({
    location: { hash: '' },
    window: { addEventListener() {} },
    Headers,
    fetch: () => new Promise(() => {}),
  });
  vm.runInContext(source, context);
  vm.runInContext(`state.session = ${JSON.stringify(session)}`, context);
  return context;
}

test('admin rail retains all authorized pages', () => {
  const app = application({ role: 'carpool_admin' });
  assert.deepEqual(Array.from(app.navigation(), entry => entry[0]), ['/', '/cars', '/admin-accounts', '/users', '/usage', '/requests', '/pricing', '/retention', '/audit', '/password']);
});

test('passenger rail does not expose administrative pages', () => {
  const app = application({ role: 'passenger' });
  assert.deepEqual(Array.from(app.navigation(), entry => entry[0]), ['/', '/members', '/accounts', '/keys', '/password']);
});

test('forced password change leaves only the security gate in either rail', () => {
  for (const role of ['carpool_admin', 'passenger']) {
    const app = application({ role, must_change_password: true });
    assert.deepEqual(Array.from(app.navigation(), entry => entry[0]), ['/password']);
  }
});

test('light entrypoint preserves external CSP-compatible resources', () => {
  const html = readFileSync(new URL('../internal/carpool/web/assets/index.html', import.meta.url), 'utf8');
  assert.match(html, /name="color-scheme" content="light dark"/);
  assert.match(html, /name="theme-color" content="#f5f4ed"/);
  assert.match(html, /src="\/assets\/app\.js"/);
  assert.doesNotMatch(html, /<style>|<script>|https?:\/\//);
});

test('navigation icons are decorative and unknown routes cannot inject markup', () => {
  const app = application({ role: 'carpool_admin' });
  for (const [route] of app.navigation()) {
    const icon = app.iconMarkup(route);
    assert.match(icon, /<svg/);
    assert.match(icon, /aria-hidden="true"/);
    assert.doesNotMatch(icon, /style=|onload=|<script/);
  }
  const hostile = '"><script>alert(1)</script>';
  assert.equal(app.iconMarkup(hostile), app.iconMarkup('/'));
  assert(!app.iconMarkup(hostile).includes(hostile));
});

for (const value of [null, undefined, '']) {
  test(`missing money ${String(value)} is not displayed as zero`, () => {
    assert.equal(application({ role: 'passenger' }).moneyMarkup(value), '未提供');
  });
}

test('known zero and exact decimal money stay distinct from missing values', () => {
  const app = application({ role: 'passenger' });
  assert.equal(app.moneyMarkup('0'), '$0');
  assert.equal(app.moneyMarkup('0.000000001'), '$0.000000001');
  assert.equal(app.moneyMarkup('<img onerror=alert(1)>'), '$&lt;img onerror=alert(1)&gt;');
});

test('billing availability is explicit, not inferred from an unknown state', () => {
  const app = application({ role: 'passenger' });
  const billing = { limit_usd: '50', used_usd: null, remaining_usd: null, status: 'unknown', usage_percent: null };
  const markup = app.billingMarkup(billing);
  assert.match(markup, /额度状态未知/);
  assert.match(markup, /未提供/);
  assert.doesNotMatch(markup, /额度可用|\$0|<progress/);
  assert.match(app.billingMarkup({ ...billing, status: 'active' }), /额度可用/);
});
