@echo off
echo ========================================
echo   Web3 AMM DEX 启动脚本
echo ========================================
echo.

:: Step 1: Compile contracts
echo [1/4] 编译智能合约...
call npx hardhat compile
if %errorlevel% neq 0 (
    echo ❌ 编译失败
    pause
    exit /b 1
)
echo ✅ 编译完成
echo.

:: Step 2: Start Hardhat node
echo [2/4] 启动本地区块链节点...
start "Hardhat Node" cmd /c "npx hardhat node"
timeout /t 5 /nobreak >nul
echo ✅ 区块链节点已启动 (端口 8545)
echo.

:: Step 3: Deploy contracts
echo [3/4] 部署智能合约...
call npx hardhat run scripts/deploy.js --network localhost
if %errorlevel% neq 0 (
    echo ❌ 部署失败
    pause
    exit /b 1
)
echo ✅ 合约部署完成
echo.

:: Step 4: Start Go backend
echo [4/4] 启动 Go 后端服务...
start "Go Backend" cmd /c "cd backend && go run main.go"
timeout /t 3 /nobreak >nul
echo ✅ 后端服务已启动 (端口 8080)
echo.

echo ========================================
echo   🚀 所有服务已启动!
echo   区块链: http://localhost:8545
echo   后端API: http://localhost:8080
echo   前端: 打开 frontend/index.html
echo ========================================
echo.

:: Open browser
echo 正在打开浏览器...
start "" "frontend\index.html"

pause
