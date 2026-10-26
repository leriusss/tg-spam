// PNG evidence from the real Go renderer export; no Telegram connection.
const { chromium } = require('../playwright/node_modules/playwright-core');
const fs = require('node:fs');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

async function main() {
  const dir = path.resolve(process.argv[2]);
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_EXECUTABLE_PATH || undefined,
  });
  try {
    const page = await browser.newPage({ viewport: { width: 420, height: 900 }, deviceScaleFactor: 1 });
    await page.goto(pathToFileURL(path.join(dir, 'examples.html')).href);
    const rows = [];
    for (const section of await page.locator('section').all()) {
      const id = await section.getAttribute('id');
      await section.screenshot({ path: path.join(dir, id + '.png') });
      const overflow = await section.evaluate(el => el.scrollWidth > el.clientWidth);
      if (overflow) throw new Error('Horizontal overflow: ' + id);
      rows.push({ id, png: id + '.png' });
    }
    const long = JSON.parse(fs.readFileSync(path.join(dir, '11-long-history.json'), 'utf8'));
    const extra = [
      ['13-long-last', long.Details.at(-1), long.DetailButtons.at(-1)],
      ['14-technical', long.Technical[0], long.TechnicalButtons[0]],
    ];
    for (const [label, text, buttons] of extra) {
      const section = page.locator('section').first();
      await section.evaluate((el, value) => {
        el.innerHTML = '<h2>' + value[0] + '</h2><article>' + value[1] + '</article><nav></nav>';
        for (const row of value[2]) {
          const line = document.createElement('div');
          for (const label of row) {
            const button = document.createElement('button');
            button.textContent = label;
            line.append(button);
          }
          el.querySelector('nav').append(line);
        }
      }, [label, text, buttons]);
      await section.screenshot({ path: path.join(dir, label + '.png') });
      rows.push({ id: label, png: label + '.png' });
    }
    fs.writeFileSync(path.join(dir, 'png-checks.json'), JSON.stringify({
      passed: true, source: 'real Go renderer fixtures', viewport: 420, screenshots: rows,
    }, null, 2));
    console.log(JSON.stringify({ passed: true, png: rows.length }));
  } finally {
    await browser.close();
  }
}
main().catch(error => { console.error(error); process.exit(1); });
