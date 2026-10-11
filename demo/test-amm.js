const { chromium } = require('playwright');

(async () => {
  const results = [];
  const log = (step, pass, detail) => {
    results.push({ step, pass, detail });
    console.log(`${pass ? '✅' : '❌'} ${step}: ${detail}`);
  };

  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const filePath = 'file:///' + __dirname.replace(/\\/g, '/') + '/index.html';

  try {
    // 1. Open the page
    await page.goto(filePath, { waitUntil: 'networkidle' });
    const title = await page.title();
    log('1. Open page', title.includes('Web3 AMM DEX'), `Title: "${title}"`);

    // 2. Take initial screenshot
    await page.screenshot({ path: __dirname + '/verify-01-initial.png', fullPage: false });
    log('2. Initial screenshot', true, 'Saved verify-01-initial.png');

    // 3. Click "连接钱包" (Connect Wallet)
    const connectBtn = page.locator('#swapBtn');
    const connectText = await connectBtn.textContent();
    log('3a. Button text before click', connectText.includes('连接钱包'), `Text: "${connectText.trim()}"`);

    await connectBtn.click();
    await page.waitForTimeout(500);

    // After connecting, if no amount entered, button should show "输入数量"
    const afterConnectText = await connectBtn.textContent();
    const walletConnected = afterConnectText.includes('输入数量') || afterConnectText.includes('交换');
    log('3b. Click 连接钱包', walletConnected, `Button now: "${afterConnectText.trim()}"`);

    // Check for toast notification
    const toastVisible = await page.locator('.toast').isVisible().catch(() => false);
    log('3c. Wallet connected toast', toastVisible, toastVisible ? 'Toast notification visible' : 'No toast (may have auto-dismissed)');

    // 4. Enter 100 in swap input
    await page.locator('#fromAmount').fill('100');
    await page.waitForTimeout(200);
    const fromVal = await page.locator('#fromAmount').inputValue();
    log('4. Enter 100 in swap input', fromVal === '100', `Input value: "${fromVal}"`);

    // 5. Verify output is calculated (~99.5)
    const toAmountStr = await page.locator('#toAmount').inputValue();
    const toAmount = parseFloat(toAmountStr);
    // AMM formula: amountInWithFee = 100 * 0.997 = 99.7
    // amountOut = (99.7 * 99800) / (100000 + 99.7) = 9950060 / 100099.7 ≈ 99.4013
    const expectedApprox = 99.4;
    const outputCorrect = toAmount > 99.0 && toAmount < 100.0;
    log('5. Output calculated', outputCorrect, `toAmount = ${toAmountStr} (expected ~${expectedApprox.toFixed(1)})`);

    // 6. Take screenshot with input filled
    await page.screenshot({ path: __dirname + '/verify-02-filled.png', fullPage: false });
    log('6. Filled screenshot', true, 'Saved verify-02-filled.png');

    // Verify button is now "交换"
    const readyText = await connectBtn.textContent();
    const buttonReady = readyText.includes('交换');
    log('6b. Swap button ready', buttonReady, `Button text: "${readyText.trim()}"`);

    // 7. Count initial tx items
    const initialTxCount = await page.locator('#txList .tx-item').count();

    // 8. Click swap button
    await connectBtn.click();
    log('7. Click swap button', true, 'Clicked #swapBtn');

    // 9. Wait 3 seconds for transaction simulation
    await page.waitForTimeout(3000);
    log('8. Waited 3s for transaction', true, 'Transaction simulation time elapsed');

    // 10. Verify transaction log has a new entry
    const finalTxCount = await page.locator('#txList .tx-item').count();
    const txAdded = finalTxCount > initialTxCount;
    log('9. Transaction log entry', txAdded, `Tx items: ${initialTxCount} → ${finalTxCount}`);

    // Check tx detail text
    if (txAdded) {
      const firstTxText = await page.locator('#txList .tx-item').first().textContent();
      log('9b. Tx detail', firstTxText.includes('100.00') && firstTxText.includes('USDT'), `Detail: "${firstTxText.trim().substring(0, 100)}"`);
    }

    // Verify balances updated
    const newFromBalance = await page.locator('#fromBalance').textContent();
    const balanceDeducted = parseFloat(newFromBalance.replace(/,/g, '')) < 10000;
    log('9c. Balance updated', balanceDeducted, `From balance: ${newFromBalance} (should be < 10,000.00)`);

    // 11. Take final screenshot
    await page.screenshot({ path: __dirname + '/verify-03-after-swap.png', fullPage: false });
    log('10. Final screenshot', true, 'Saved verify-03-after-swap.png');

    // Take full page screenshot
    await page.screenshot({ path: __dirname + '/verify-04-fullpage.png', fullPage: true });
    log('10b. Full page screenshot', true, 'Saved verify-04-fullpage.png');

  } catch (err) {
    log('FATAL ERROR', false, err.message);
    await page.screenshot({ path: __dirname + '/verify-error.png', fullPage: false }).catch(() => {});
  } finally {
    await browser.close();
  }

  // Summary
  console.log('\n' + '='.repeat(60));
  console.log('TEST SUMMARY');
  console.log('='.repeat(60));
  const passed = results.filter(r => r.pass).length;
  const failed = results.filter(r => !r.pass).length;
  results.forEach(r => {
    console.log(`  ${r.pass ? '✅' : '❌'} ${r.step}: ${r.detail}`);
  });
  console.log(`\n  Total: ${passed} passed, ${failed} failed out of ${results.length}`);
  console.log('='.repeat(60));
  process.exit(failed > 0 ? 1 : 0);
})();
