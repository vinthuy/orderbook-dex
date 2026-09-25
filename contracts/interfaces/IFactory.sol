// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

interface IFactory {
    event PoolCreated(
        address indexed token0,
        address indexed token1,
        address pool,
        uint256 poolId
    );

    function createPool(address token0, address token1) external returns (address pool);
    function getPool(address token0, address token1) external view returns (address pool);
    function getAllPools() external view returns (address[] memory pools);
}
