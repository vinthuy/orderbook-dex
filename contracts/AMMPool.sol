// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

import "./Token.sol";

contract AMMPool {
    Token public token0;
    Token public token1;
    uint256 public reserve0;
    uint256 public reserve1;
    uint256 public totalSupply;
    mapping(address => uint256) public liquidityBalance;

    uint256 public constant FEE_DIVISOR = 10000;
    uint256 public constant FEE_RATE = 30;

    event Swap(address indexed sender, uint256 amount0In, uint256 amount1In, uint256 amount0Out, uint256 amount1Out);
    event Mint(address indexed to, uint256 liquidity);
    event Burn(address indexed to, uint256 amount0, uint256 amount1);
    event Sync(uint256 reserve0, uint256 reserve1);

    constructor(Token _token0, Token _token1) {
        token0 = _token0;
        token1 = _token1;
    }

    function getReserves() public view returns (uint256 r0, uint256 r1) {
        return (reserve0, reserve1);
    }

    function getAmountOut(uint256 amountIn, uint256 reserveIn, uint256 reserveOut) public pure returns (uint256) {
        require(amountIn > 0, "Invalid input");
        require(reserveIn > 0 && reserveOut > 0, "Invalid reserves");
        uint256 amountInWithFee = amountIn * (FEE_DIVISOR - FEE_RATE);
        uint256 numerator = amountInWithFee * reserveOut;
        uint256 denominator = (reserveIn * FEE_DIVISOR) + amountInWithFee;
        return numerator / denominator;
    }

    function mint(address to) external returns (uint256 liquidity) {
        uint256 r0 = reserve0;
        uint256 r1 = reserve1;
        uint256 amount0 = token0.balanceOf(address(this));
        uint256 amount1 = token1.balanceOf(address(this));
        require(amount0 > 0 && amount1 > 0, "Need tokens");

        if (totalSupply == 0) {
            liquidity = sqrt(amount0 * amount1) - 1;
        } else {
            liquidity = min((amount0 * totalSupply) / r0, (amount1 * totalSupply) / r1);
        }
        require(liquidity > 0, "Liquidity zero");

        totalSupply += liquidity;
        liquidityBalance[to] += liquidity;
        emit Mint(to, liquidity);
        _sync();
    }

    function burn(address to) external returns (uint256 amount0, uint256 amount1) {
        uint256 r0 = reserve0;
        uint256 r1 = reserve1;
        uint256 liquidity = liquidityBalance[msg.sender];
        require(liquidity > 0, "No liquidity");

        amount0 = (liquidity * r0) / totalSupply;
        amount1 = (liquidity * r1) / totalSupply;
        require(amount0 > 0 && amount1 > 0, "Nothing to burn");

        liquidityBalance[msg.sender] = 0;
        totalSupply -= liquidity;
        emit Burn(to, amount0, amount1);
        token0.transfer(to, amount0);
        token1.transfer(to, amount1);
        _sync();
    }

    function swap(address tokenIn, uint256 amountIn, uint256 amountOutMin, address to) external {
        require(amountIn > 0, "Invalid input");
        require(amountOutMin > 0, "Min output required");

        bool isToken0 = (tokenIn == address(token0));
        uint256 r0 = reserve0;
        uint256 r1 = reserve1;

        if (isToken0) {
            uint256 amountOut = getAmountOut(amountIn, r0, r1);
            require(amountOut >= amountOutMin, "Slippage");
            require(token0.transferFrom(msg.sender, address(this), amountIn), "Transfer0 failed");
            token1.transfer(to, amountOut);
            reserve0 = r0 + amountIn;
            reserve1 = r1 - amountOut;
        } else {
            uint256 amountOut = getAmountOut(amountIn, r1, r0);
            require(amountOut >= amountOutMin, "Slippage");
            require(token1.transferFrom(msg.sender, address(this), amountIn), "Transfer1 failed");
            token0.transfer(to, amountOut);
            reserve1 = r1 + amountIn;
            reserve0 = r0 - amountOut;
        }

        emit Swap(msg.sender, 0, 0, 0, 0);
        emit Sync(reserve0, reserve1);
    }

    function removeLiquidity(uint256 liquidity, address to) external returns (uint256 amount0, uint256 amount1) {
        require(liquidityBalance[msg.sender] >= liquidity, "Insufficient liquidity");
        uint256 r0 = reserve0;
        uint256 r1 = reserve1;

        amount0 = (liquidity * r0) / totalSupply;
        amount1 = (liquidity * r1) / totalSupply;
        require(amount0 > 0 && amount1 > 0, "Nothing to withdraw");

        liquidityBalance[msg.sender] -= liquidity;
        totalSupply -= liquidity;
        token0.transfer(to, amount0);
        token1.transfer(to, amount1);
        _sync();
    }

    function _sync() internal {
        emit Sync(reserve0, reserve1);
    }

    function sqrt(uint256 y) internal pure returns (uint256 z) {
        if (y > 3) {
            z = y;
            uint256 x = y / 2;
            while (x < z) {
                z = x;
                x = (y / x + x) / 2;
            }
        } else if (y != 0) {
            z = 1;
        }
    }

    function min(uint256 a, uint256 b) internal pure returns (uint256) {
        return a < b ? a : b;
    }
}
