// Render a promo page frame-by-frame to an MP4 in out/.
// Usage: NODE_PATH=$(npm root -g) node render.cjs [--page index.html] [--name cospace-promo]
//        [--audio music.wav] [--stills 3,12,...] [--fps 30]
// The page sets window.__size = [w, h] when it is not 1920×1080 (e.g. vertical).
const { chromium } = require('playwright');
const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');

const args = process.argv.slice(2);
const opt = (k, d) => { const i = args.indexOf(k); return i >= 0 ? args[i + 1] : d; };
const FPS = +opt('--fps', 30);
const WORKERS = +opt('--workers', 6);
const stills = opt('--stills');
const PAGE = opt('--page', 'index.html');
const NAME = opt('--name', 'cospace-promo');
const AUDIO = opt('--audio', 'music.wav');
const out = path.join(__dirname, 'out');
const url = 'file://' + path.join(__dirname, PAGE) + '?render';

async function page(browser) {
  const p = await browser.newPage({ viewport: { width: 1920, height: 1080 } });
  await p.goto(url);
  const size = await p.evaluate(() => window.__size);
  if (size) await p.setViewportSize({ width: size[0], height: size[1] });
  await p.evaluate(() => document.fonts.ready);
  await p.waitForFunction(() => [...document.images].every(i => i.complete && i.naturalWidth));
  await p.waitForTimeout(300);
  return p;
}

(async () => {
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch();
  if (stills) {
    const p = await page(browser);
    for (const t of stills.split(',').map(Number)) {
      await p.evaluate(t => window.__render(t), t);
      await p.screenshot({ path: path.join(out, `${NAME === 'cospace-promo' ? 'still' : NAME}-${t}.png`) });
    }
    await browser.close();
    return;
  }
  const p0 = await page(browser);
  const dur = await p0.evaluate(() => window.__duration);
  const total = Math.round(dur * FPS);
  const dir = path.join(out, 'frames-' + NAME);
  fs.rmSync(dir, { recursive: true, force: true });
  fs.mkdirSync(dir, { recursive: true });
  const pages = [p0, ...(await Promise.all(Array.from({ length: WORKERS - 1 }, () => page(browser))))];
  let next = 0, done = 0;
  await Promise.all(pages.map(async p => {
    while (next < total) {
      const i = next++;
      await p.evaluate(t => window.__render(t), i / FPS);
      await p.screenshot({ path: path.join(dir, `f${String(i).padStart(5, '0')}.jpg`), type: 'jpeg', quality: 94 });
      if (++done % 150 === 0) console.log(`${done}/${total}`);
    }
  }));
  await browser.close();
  const audio = path.join(out, AUDIO);
  const ff = ['-y', '-framerate', String(FPS), '-i', path.join(dir, 'f%05d.jpg')];
  if (fs.existsSync(audio)) ff.push('-i', audio, '-c:a', 'aac', '-b:a', '192k', '-shortest');
  ff.push('-c:v', 'libx264', '-preset', 'slow', '-crf', '18', '-pix_fmt', 'yuv420p', '-movflags', '+faststart', path.join(out, NAME + '.mp4'));
  await new Promise((res, rej) => spawn('ffmpeg', ff, { stdio: ['ignore', 'ignore', 'inherit'] }).on('exit', c => c ? rej(new Error('ffmpeg ' + c)) : res()));
  fs.rmSync(dir, { recursive: true, force: true });
  console.log(`wrote out/${NAME}.mp4`);
})();
