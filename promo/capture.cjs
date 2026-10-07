// Screenshot the real console and invite page against demo data, into ui/.
// Usage: NODE_PATH=$(npm root -g) node capture.cjs
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..');
const ORIGIN = 'http://cospace.local';
const OUT = path.join(__dirname, 'ui');
const W = 1280, H = 760;

const day = d => new Date(Date.now() - d * 864e5).toISOString().slice(0, 10);
const ago = d => new Date(Date.now() - d * 864e5).toISOString();
const base = {
  ip: '192.168.64.3', manual_sleep: false, memory_gb: 2, cpus: 4, providers: ['anthropic', 'openai', 'xai'],
  usage_today: { date: day(0), requests: 0, tokens_in: 0, tokens_out: 0 }, spent_usd: 0, usd_limit: 0, max_concurrency: 5,
  full_auto: true, codex_route: 'official', codex_model: 'gpt-6-astra', network_mode: 'open', network_upgrading: false, network_control_ready: true,
};
const FP = { mei: 'u4Rk9Lq2', leo: 'Hc7Tz0Wn', sam: 'p3XbJ8Vd', ivy: 'Ke5Ns1Ym', alice: 'x9GfQ2aL', bob: 'Tr6Wm4Pc' };
const member = (name, d) => ({ name, pub_key: 'ssh-ed25519 AAAA…', fingerprint: `SHA256:${FP[name]}…`, added_at: ago(d) });
const others = [
  { ...base, name: 'thesis-lab', state: 'stopped', ip: '', created_at: ago(20), host_key_fp: 'SHA256:Qm4tYb0d8cXo2LwJrS7nVd1kFz9pHe3aUg6iTy5wN0E', members: [member('mei', 18)], spent_usd: 6.2 },
  { ...base, name: 'hackathon', state: 'running', created_at: ago(9), host_key_fp: 'SHA256:Lr8vNc2pXb5mKa1sQ7dWe4tYh9zUo3iFg6jRk0cTnM2', members: [member('leo', 8), member('sam', 8), member('ivy', 7)], spent_usd: 31.5, usage_today: { date: day(0), requests: 212, tokens_in: 3.1e6, tokens_out: 4.2e5 } },
];
const shopfront = { ...base, name: 'shopfront', state: 'running', created_at: ago(0), host_key_fp: 'SHA256:ZIu6DjaSVGqZC8d/KySWT77OKbgPM9q7P6SkT+th2bU', members: [] };
const shopfrontLive = { ...shopfront, created_at: ago(13), members: [member('alice', 12), member('bob', 11)], spent_usd: 18.4, usd_limit: 20,
  usage_today: { date: day(0), requests: 318, tokens_in: 5.2e6, tokens_out: 6.1e5 } };
const usage = Array.from({ length: 14 }, (_, i) => {
  const t = [0.8, 1.2, 0.6, 1.9, 2.4, 1.1, 0.4, 2.8, 3.3, 2.1, 3.9, 2.6, 4.4, 5.8][i] * 1e6;
  return { date: day(13 - i), requests: Math.round(t / 16000), tokens_in: Math.round(t * 0.9), tokens_out: Math.round(t * 0.1) };
});
const INSTALL = { posix: 'curl -fsSL https://cospace.jingxuan.uk/install.sh | sh', windows: 'irm https://raw.githubusercontent.com/JingxuanKang/cospace/master/docs/install.ps1 | iex' };
const invite = { code: '7KQ4-M9XT', command: 'cospace pair tc1:9hQx…mK2a 7KQ4-M9XT', expires_at: new Date(Date.now() + 600e3).toISOString(), install: INSTALL };

let spaces = others;
const json = body => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });

function invitePage() {
  let s = fs.readFileSync(path.join(ROOT, 'internal/api/invite.html'), 'utf8');
  s = s.replace(/\{\{if \.Dead\}\}[\s\S]*?\{\{else\}\}/, '').replace('{{if not .Dead}}', '').replace(/\{\{end\}\}/g, '');
  s = s.replace('{{.InstallPOSIX}},', JSON.stringify(INSTALL.posix) + ',').replace('{{.InstallWindows}}', JSON.stringify(INSTALL.windows))
    .replaceAll('{{.InstallPOSIX}}', INSTALL.posix);
  return s.replaceAll('{{.Space}}', 'shopfront').replaceAll('{{.PairCommand}}', invite.command)
    .replaceAll('{{.MinutesLeft}}', '10').replace('{{printf "%q" .Space}}', '"shopfront"');
}

async function route(r) {
  const u = new URL(r.request().url());
  const p = u.pathname;
  if (p === '/') return r.fulfill({ status: 200, contentType: 'text/html', body: fs.readFileSync(path.join(ROOT, 'internal/api/web/index.html')) });
  if (p.startsWith('/i/')) return r.fulfill({ status: 200, contentType: 'text/html', body: invitePage() });
  if (p === '/api/host') return r.fulfill(json({ awake: spaces.filter(s => s.state === 'running').length, memory_reserved_gb: 2 * spaces.length, memory_total_gb: 32, memory_used_gb: 14, on_battery: false, spaces: spaces.length }));
  if (p === '/api/spaces' && r.request().method() === 'POST') { spaces = [...others, shopfront]; return r.fulfill(json(shopfront)); }
  if (p === '/api/spaces') return r.fulfill(json(spaces));
  if (p === '/api/sponsor') return r.fulfill(json({ anthropic: { enabled: true, cred_present: true }, openai: { enabled: true, cred_present: true }, xai: { enabled: true, cred_present: true } }));
  if (p === '/api/templates') return r.fulfill(json([]));
  if (p === '/api/codex-options') return r.fulfill(json({ models: ['gpt-6-astra', 'gpt-5.6-sol'], routes: [{ id: 'official', label: 'Official account', configured: true }] }));
  if (p.endsWith('/usage')) return r.fulfill(json(spaces.includes(shopfrontLive) ? usage : []));
  if (p.endsWith('/invites')) return r.fulfill(json(invite));
  return r.fulfill(json({}));
}

// Keep promo frames free of any real hostname.
const scrub = page => page.evaluate(() => {
  const w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let n; (n = w.nextNode());) {
    n.nodeValue = n.nodeValue.replace(/https:\/\/[a-z0-9.]+\/(?:JingxuanKang\/cospace\/master\/docs\/|cospace\/)?/g, '…/').replace(/http:\/\/cospace\.local/g, '…');
  }
});
const box = async loc => { const b = await loc.boundingBox(); return { x: b.x + b.width / 2, y: b.y + b.height / 2 }; };
const shot = async (page, name) => { await page.waitForTimeout(900); await scrub(page); await page.screenshot({ path: path.join(OUT, name + '.png') }); };

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ viewport: { width: W, height: H }, deviceScaleFactor: 2 });
  await ctx.route(`${ORIGIN}/**`, route);
  const page = await ctx.newPage();
  const pts = {};

  await page.goto(ORIGIN + '/');
  await shot(page, 'overview');
  const newBtn = page.getByRole('button', { name: /New Space/ }).last();
  pts.newSpace = await box(newBtn);
  await newBtn.click();
  await page.waitForTimeout(500);
  await page.locator('#new-space-name').fill('shopfront');
  await shot(page, 'create');
  const submit = page.locator('.modal button[type=submit]');
  pts.create = await box(submit);
  pts.nameField = await box(page.locator('#new-space-name'));
  await submit.click();
  await page.waitForTimeout(600);
  await page.mouse.move(0, 0);
  await shot(page, 'detail');
  const inv = page.getByRole('button', { name: /Invite a Guest/ }).first();
  pts.invite = await box(inv);
  await inv.click();
  await shot(page, 'invite');
  pts.copyLink = await box(page.locator('[data-action=copy-invite-link]'));

  spaces = [...others, shopfrontLive];
  await page.goto(ORIGIN + '/');
  await page.waitForTimeout(400);
  await page.locator('.nav-item[data-space=shopfront]').click();
  await page.mouse.move(0, 0);
  await shot(page, 'live');

  const g = await ctx.newPage();
  await g.setViewportSize({ width: 1100, height: 760 });
  await g.goto(ORIGIN + '/i/7KQ4-M9XT');
  await g.waitForTimeout(600);
  await scrub(g);
  pts.copyAll = await box(g.locator('#copy-all'));
  pts.apps = await box(g.locator('.apps-grid'));
  await g.screenshot({ path: path.join(OUT, 'invite-page.png'), fullPage: true });

  fs.writeFileSync(path.join(OUT, 'points.js'), `window.PTS = ${JSON.stringify(pts)};\n`);
  await browser.close();
  console.log(pts);
})();
