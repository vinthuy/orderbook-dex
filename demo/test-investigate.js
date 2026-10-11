const { chromium } = require('playwright');

(async () => {
    const browser = await chromium.launch({ headless: true });
    const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
    
    const errors = [];
    const pageErrors = [];
    page.on('console', msg => { if (msg.type() === 'error') errors.push(msg.text()); });
    page.on('pageerror', err => { pageErrors.push(err.message); });
    
    console.log('=== Web3 AMM DEX Demo - Bug Investigation ===\n');
    
    // Load page
    await page.goto('file:///D:/code/web3/demo/index.html');
    await page.waitForTimeout(500);
    await page.screenshot({ path: 'D:/code/web3/demo/screenshot-initial.png', fullPage: true });
    
    // === BUG #1: Token Picker overlay blocks ALL clicks ===
    console.log('=== BUG #1: Hidden Token Picker Overlay Blocking Clicks ===');
    
    // Check the tokenPicker element's state
    const pickerState = await page.evaluate(() => {
        const picker = document.getElementById('tokenPicker');
        const style = window.getComputedStyle(picker);
        return {
            classList: Array.from(picker.classList),
            display: style.display,
            visibility: style.visibility,
            position: style.position,
            zIndex: style.zIndex,
            top: style.top,
            left: style.left,
            right: style.right,
            bottom: style.bottom,
            width: style.width,
            height: style.height,
            pointerEvents: style.pointerEvents,
            opacity: style.opacity,
            boundingRect: picker.getBoundingClientRect()
        };
    });
    console.log('Token Picker element state:');
    console.log('  classList:', pickerState.classList);
    console.log('  display:', pickerState.display);
    console.log('  visibility:', pickerState.visibility);
    console.log('  position:', pickerState.position);
    console.log('  zIndex:', pickerState.zIndex);
    console.log('  top:', pickerState.top);
    console.log('  opacity:', pickerState.opacity);
    console.log('  pointerEvents:', pickerState.pointerEvents);
    console.log('  boundingRect:', JSON.stringify(pickerState.boundingRect));
    
    // The CSS says .hidden { display: none; } BUT the element has inline style="position:fixed;..." 
    // which might override. Let's check
    const inlineStyle = await page.evaluate(() => {
        return document.getElementById('tokenPicker').getAttribute('style');
    });
    console.log('\n  inline style attribute:', inlineStyle);
    
    // === BUG #2: CalculateOutput not working ===
    console.log('\n=== BUG #2: Swap Calculation via evaluate (bypassing click) ===');
    const calcResult = await page.evaluate(() => {
        try {
            document.getElementById('fromAmount').value = '100';
            calculateOutput();
            return {
                toAmount: document.getElementById('toAmount').value,
                swapBtnText: document.getElementById('swapBtn').textContent.trim(),
                swapBtnClass: document.getElementById('swapBtn').className,
                priceInfo: document.getElementById('priceInfo').textContent,
                error: null
            };
        } catch(e) {
            return { error: e.message };
        }
    });
    console.log('  toAmount:', calcResult.toAmount);
    console.log('  swapBtnText:', calcResult.swapBtnText);
    console.log('  swapBtnClass:', calcResult.swapBtnClass);
    console.log('  priceInfo:', calcResult.priceInfo);
    console.log('  error:', calcResult.error);
    
    await page.screenshot({ path: 'D:/code/web3/demo/screenshot-after-calc.png', fullPage: true });
    
    // === Test Connect Wallet via evaluate ===
    console.log('\n=== BUG #3: Connect Wallet via evaluate ===');
    const connectResult = await page.evaluate(() => {
        try {
            executeSwap();  // This is the connect wallet function when not connected
            return {
                connected: state.connected,
                swapBtnText: document.getElementById('swapBtn').textContent.trim(),
                swapBtnClass: document.getElementById('swapBtn').className,
                error: null
            };
        } catch(e) {
            return { error: e.message };
        }
    });
    console.log('  connected:', connectResult.connected);
    console.log('  swapBtnText:', connectResult.swapBtnText);
    console.log('  swapBtnClass:', connectResult.swapBtnClass);
    console.log('  error:', connectResult.error);
    
    // === Test Execute Swap ===
    console.log('\n=== BUG #4: Execute Swap ===');
    const swapResult = await page.evaluate(async () => {
        try {
            document.getElementById('fromAmount').value = '100';
            calculateOutput();
            await executeSwap();
            const txList = document.getElementById('txList').innerHTML;
            return {
                txHasContent: txList.includes('0x'),
                txSnippet: txList.substring(0, 200),
                fromBalance: document.getElementById('fromBalance').textContent,
                toBalance: document.getElementById('toBalance').textContent,
                error: null
            };
        } catch(e) {
            return { error: e.message, stack: e.stack };
        }
    });
    console.log('  txHasContent:', swapResult.txHasContent);
    console.log('  txSnippet:', swapResult.txSnippet);
    console.log('  fromBalance:', swapResult.fromBalance);
    console.log('  toBalance:', swapResult.toBalance);
    console.log('  error:', swapResult.error);
    
    await page.screenshot({ path: 'D:/code/web3/demo/screenshot-after-swap.png', fullPage: true });
    
    // === Test Tabs ===
    console.log('\n=== Test Tab Switching ===');
    const tabResult = await page.evaluate(() => {
        try {
            // Simulate clicking the liquidity tab
            const tabs = document.querySelectorAll('.tab');
            tabs[1].click();
            const liqVisible = !document.getElementById('liquidityTab').classList.contains('hidden');
            const swapHidden = document.getElementById('swapTab').classList.contains('hidden');
            return { liqVisible, swapHidden, error: null };
        } catch(e) {
            return { error: e.message };
        }
    });
    console.log('  liquidityTab visible:', tabResult.liqVisible);
    console.log('  swapTab hidden:', tabResult.swapHidden);
    console.log('  error:', tabResult.error);
    
    // === Test SetMax ===
    console.log('\n=== Test SetMax ===');
    const maxResult = await page.evaluate(() => {
        try {
            // Reset to USDT->DAI
            state.fromToken = 'USDT';
            state.toToken = 'DAI';
            document.getElementById('fromTokenSymbol').textContent = 'USDT';
            document.getElementById('toTokenSymbol').textContent = 'DAI';
            updateBalances();
            setMaxFrom();
            return {
                fromAmount: document.getElementById('fromAmount').value,
                toAmount: document.getElementById('toAmount').value,
                fromBalance: document.getElementById('fromBalance').textContent,
                toBalance: document.getElementById('toBalance').textContent,
                error: null
            };
        } catch(e) {
            return { error: e.message };
        }
    });
    console.log('  fromAmount:', maxResult.fromAmount);
    console.log('  toAmount:', maxResult.toAmount);
    console.log('  error:', maxResult.error);
    
    // === Test Token Picker ===
    console.log('\n=== Test Token Picker ===');
    const pickerResult = await page.evaluate(() => {
        try {
            toggleTokenPicker('from');
            const picker = document.getElementById('tokenPicker');
            const tokens = document.getElementById('tokenList').children;
            return {
                pickerVisible: !picker.classList.contains('hidden'),
                tokenCount: tokens.length,
                error: null
            };
        } catch(e) {
            return { error: e.message };
        }
    });
    console.log('  pickerVisible:', pickerResult.pickerVisible);
    console.log('  tokenCount:', pickerResult.tokenCount);
    console.log('  error:', pickerResult.error);
    
    await page.screenshot({ path: 'D:/code/web3/demo/screenshot-token-picker.png', fullPage: true });
    
    // === Errors ===
    console.log('\n=== Console Errors ===');
    if (errors.length > 0) errors.forEach(e => console.log('  ❌', e));
    else console.log('  ✅ No console errors');
    
    if (pageErrors.length > 0) {
        console.log('\n=== Page Errors ===');
        pageErrors.forEach(e => console.log('  ❌', e));
    } else {
        console.log('  ✅ No page errors');
    }
    
    await browser.close();
    console.log('\n=== Investigation Complete ===');
})();
