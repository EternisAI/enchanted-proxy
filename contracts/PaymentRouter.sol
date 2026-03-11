// SPDX-License-Identifier: MIT
// Joel: I used this to deploy a contract for accepting FAI payments on 3/2026 so I could test (with real FAI) on Silo Staging
// not sure if we need these for anything else, but leaving here in repo in case useful to future self / others
pragma solidity ^0.8.26;

contract PaymentRouter {
    address public owner;
    address public operator;
    mapping(address => bool) public isWhitelisted;

    event PaymentReceived(bytes32 paymentId, address token, uint256 amount, address from);

    modifier onlyOwner() {
        require(msg.sender == owner, "Only owner");
        _;
    }

    constructor() {
        owner = msg.sender;
    }

    receive() external payable {
        revert("no receive");
    }

    fallback() external {
        revert("no fallback");
    }

    function setOwner(address _owner) external onlyOwner {
        owner = _owner;
    }

    function setOperator(address _operator) external onlyOwner {
        operator = _operator;
    }

    function setWhitelisted(address token, bool status) external onlyOwner {
        isWhitelisted[token] = status;
    }

    function pay(address token, uint256 amount, bytes32 paymentId) external {
        require(isWhitelisted[token], "Not whitelisted");
        _safeTransferFrom(token, msg.sender, address(this), amount);
        emit PaymentReceived(paymentId, token, amount, msg.sender);
    }

    function withdraw(address token, address to, uint256 amount) external onlyOwner {
        _safeTransfer(token, to, amount);
    }

    function _safeTransferFrom(address token, address from, address to, uint256 amount) internal {
        (bool success, bytes memory data) = token.call(
            abi.encodeWithSelector(0x23b872dd, from, to, amount)
        );
        require(success && (data.length == 0 || abi.decode(data, (bool))), "TransferFrom failed");
    }

    function _safeTransfer(address token, address to, uint256 amount) internal {
        (bool success, bytes memory data) = token.call(
            abi.encodeWithSelector(0xa9059cbb, to, amount)
        );
        require(success && (data.length == 0 || abi.decode(data, (bool))), "Transfer failed");
    }
}
