const { chromium } = require('playwright');

(async () => {
    const browser = await chromium.launch({ headless: true });
    const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
    
    // Collect console errors
    const errors = [];
    const allLogs = [];
    page.on('console', msg => {
        const entry = { type: msg.type(), text: msg.text() };
        allLogs.push(entry);
        if (msg.type() === 'error') errors.push(msg.text());
    });
    
    // Collect page errors (uncaught exceptions)
    const pageErrors = [];
    page.on('pageerror', err => {
        pageErrors.push(err.message);
    });
    
    console.log('=== Web3 AMM DEX Demo Test ===\n');
    
    // 1. Load the page
    console.log('1. Loading page...');
    await page.goto('file:///D:/code/web3/demo/index.html');
    await page.waitForTimeout(500);
    await page.screenshot({ path: 'D:/code/web3/demo/screenshot-initial.png', fullPage: true });
    console.log('   Page loaded. Screenshot saved: screenshot-initial.png');
    
    // Check initial state
    const initialBtn = await page.textContent('#swapBtn');
    const fromAmount = await page.inputValue('#fromAmount');
    const toAmount = await page.inputValue('#toAmount');
    console.log(`   Initial button text: "${initialBtn}"`);
    console.log(`   Initial fromAmount: "${fromAmount}"`);
    console.log(`   Initial toAmount: "${toAmount}"`);
    
    // 2. Test Connect Wallet
    console.log('\n2. Testing Connect Wallet...');
    try {
        await page.click('#swapBtn');
        await page.waitForTimeout(600);
        const btnAfterConnect = await page.textContent('#swapBtn');
        console.log(`   Button text after click: "${btnAfterConnect}"`);
        if (btnAfterConnect === '已连接' || btnAfterConnect.includes('已连接')) {
            console.log('   ✅ Connect wallet: PASSED');
        } else if (btnAfterConnect === '交换') {
            console.log('   ✅ Connect wallet: PASSED (state connected, button shows 交换)');
        } else {
            console.log('   ⚠️ Connect wallet: Button text unexpected: ' + btnAfterConnect);
        }
        
        // Check if toast appeared
        const toast = await page.$('.toast');
        if (toast) {
            const toastText = await toast.textContent();
            console.log(`   Toast message: ${toastText.trim().substring(0, 60)}`);
        }
    } catch (e) {
        console.log(`   ❌ Connect wallet: FAILED - ${e.message}`);
    }
    
    // 3. Test swap calculation
    console.log('\n3. Testing Swap Calculation...');
    try {
        await page.fill('#fromAmount', '100');
        await page.waitForTimeout(400);
        const toAmountVal = await page.inputValue('#toAmount');
        const btnText = await page.textContent('#swapBtn');
        console.log(`   Input: 100 ${await page.textContent('#fromTokenSymbol')}`);
        console.log(`   Output: ${toAmountVal} ${await page.textContent('#toTokenSymbol')}`);
        console.log(`   Button: "${btnText}"`);
        
        if (toAmountVal && parseFloat(toAmountVal) > 0) {
            console.log('   ✅ Swap calculation: PASSED');
        } else {
            console.log('   ❌ Swap calculation: FAILED - output is empty or zero');
        }
        
        // Check price info
        const priceInfo = await page.textContent('#priceInfo');
        console.log(`   Price info: ${priceInfo}`);
    } catch (e) {
        console.log(`   ❌ Swap calculation: FAILED - ${e.message}`);
    }
    
    await page.screenshot({ path: 'D:/code/web3/demo/screenshot-after-input.png', fullPage: true });
    
    // 4. Test execute swap
    console.log('\n4. Testing Execute Swap...');
    try {
        const btnTextBefore = await page.textContent('#swapBtn');
        console.log(`   Button before swap: "${btnTextBefore}"`);
        
        if (btnTextBefore === '交换') {
            await page.click('#swapBtn');
            await page.waitForTimeout(3000);  // Wait for simulated tx delay
            
            const txList = await page.textContent('#txList');
            const btnTextAfter = await page.textContent('#swapBtn');
            console.log(`   Button after swap: "${btnTextAfter}"`);
            
            if (txList && txList.includes('0x')) {
                console.log('   ✅ Transaction log: PASSED - tx hash found');
                console.log(`   TX log content: ${txList.trim().substring(0, 100)}...`);
            } else {
                console.log('   ❌ Transaction log: FAILED - no tx hash found');
                console.log(`   TX list content: "${txList}"`);
            }
            
            // Check if balances updated
            const fromBal = await page.textContent('#fromBalance');
            const toBal = await page.textContent('#toBalance');
            console.log(`   From balance: ${fromBal}`);
            console.log(`   To balance: ${toBal}`);
        } else {
            console.log('   ⚠️ Swap button not ready (not showing "交换"), skipping swap execution');
        }
    } catch (e) {
        console.log(`   ❌ Execute swap: FAILED - ${e.message}`);
    }
    
    await page.screenshot({ path: 'D:/code/web3/demo/screenshot-after-swap.png', fullPage: true });
    
    // 5. Test token swap (arrow button)
    console.log('\n5. Testing Token Swap (arrow)...');
    try {
        const fromTokenBefore = await page.textContent('#fromTokenSymbol');
        const toTokenBefore = await page.textContent('#toTokenSymbol');
        console.log(`   Before: ${fromTokenBefore} → ${toTokenBefore}`);
        
        await page.click('.swap-arrow');
        await page.waitForTimeout(300);
        
        const fromTokenAfter = await page.textContent('#fromTokenSymbol');
        const toTokenAfter = await page.textContent('#toTokenSymbol');
        console.log(`   After:  ${fromTokenAfter} → ${toTokenAfter}`);
        
        if (fromTokenBefore !== fromTokenAfter && toTokenBefore !== toTokenAfter) {
            console.log('   ✅ Token swap: PASSED');
        } else {
            console.log('   ❌ Token swap: FAILED - tokens did not swap');
        }
    } catch (e) {
        console.log(`   ❌ Token swap: FAILED - ${e.message}`);
    }
    
    // 6. Test token picker
    console.log('\n6. Testing Token Picker...');
    try {
        await page.click('#fromTokenSelect');
        await page.waitForTimeout(300);
        
        const picker = await page.$('#tokenPicker');
        const isHidden = await picker.evaluate(el => el.classList.contains('hidden'));
        
        if (!isHidden) {
            console.log('   ✅ Token picker opened: PASSED');
            
            // Check if tokens are listed
            const tokenItems = await page.$$('#tokenList > div');
            console.log(`   Token count: ${tokenItems.length}`);
            
            // Close it
            await page.click('#tokenPicker span[onclick="closeTokenPicker()"]');
            await page.waitForTimeout(200);
        } else {
            console.log('   ❌ Token picker: FAILED - modal not visible');
        }
    } catch (e) {
        console.log(`   ❌ Token picker: FAILED - ${e.message}`);
    }
    
    // 7. Test tab switching
    console.log('\n7. Testing Tab Switching...');
    try {
        await page.click('.tab:nth-child(2)');
        await page.waitForTimeout(300);
        
        const liquidityVisible = await page.evaluate(() => !document.getElementById('liquidityTab').classList.contains('hidden'));
        const swapVisible = await page.evaluate(() => !document.getElementById('swapTab').classList.contains('hidden'));
        
        if (liquidityVisible && !swapVisible) {
            console.log('   ✅ Tab switching: PASSED');
        } else {
            console.log('   ❌ Tab switching: FAILED');
        }
        
        // Switch back
        await page.click('.tab:nth-child(1)');
        await page.waitForTimeout(200);
    } catch (e) {
        console.log(`   ❌ Tab switching: FAILED - ${e.message}`);
    }
    
    // 8. Test setMax
    console.log('\n8. Testing SetMax...');
    try {
        // Switch back to USDT-DAI pair first
        await page.click('.swap-arrow');
        await page.waitForTimeout(200);
        await page.click('.swap-arrow');
        await page.waitForTimeout(200);
        
        await page.click('.balance-text');
        await page.waitForTimeout(300);
        
        const fromAmountVal = await page.inputValue('#fromAmount');
        const toAmountVal = await page.inputValue('#toAmount');
        console.log(`   SetMax from: ${fromAmountVal}`);
        console.log(`   Calculated to: ${toAmountVal}`);
        
        if (parseFloat(fromAmountVal) > 0 && parseFloat(toAmountVal) > 0) {
            console.log('   ✅ SetMax: PASSED');
        } else {
            console.log('   ❌ SetMax: FAILED');
        }
    } catch (e) {
        console.log(`   ❌ SetMax: FAILED - ${e.message}`);
    }
    
    await page.screenshot({ path: 'D:/code/web3/demo/screenshot-final.png', fullPage: true });
    
    // Report errors
    console.log('\n=== Console Errors ===');
    if (errors.length > 0) {
        errors.forEach(e => console.log(`  ❌ ${e}`));
    } else {
        console.log('  ✅ No console errors');
    }
    
    if (pageErrors.length > 0) {
        console.log('\n=== Page Errors (Uncaught Exceptions) ===');
        pageErrors.forEach(e => console.log(`  ❌ ${e}`));
    } else {
        console.log('  ✅ No page errors');
    }
    
    console.log('\n=== All Console Logs ===');
    allLogs.forEach(l => console.log(`  [${l.type}] ${l.text}`));
    
    await browser.close();
    console.log('\n=== Test Complete ===');
})();
