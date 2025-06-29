#!/bin/bash

# 设置超时时间（秒）
TIMEOUT=30

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "🔍 开始逐个运行测试函数，查找死锁问题..."
echo "⏰ 设置超时时间: ${TIMEOUT}秒"
echo "=========================="

# 测试函数列表
declare -a tests=(
    "TestStringToBytes"
    "Test_secWebSocketAccept"
    "Test_secWebSocketAcceptVal"
    "TestUpgradeInner"
    "Test_Server_HandshakeFail"
    "Test_DefaultCallback"
    "Test_ClientOption"
    "Test_CommonOption"
    "Test_Client_Dial_Check_Header"
    "Test_Conn"
    "Test_ReadMessage"
    "TestFragmentFrame"
    "Test_WriteControl"
    "Test_API"
    "TestPingPongClose"
)

# 记录结果
passed_tests=()
failed_tests=()
timeout_tests=()

# 运行单个测试函数
run_test() {
    local test_func=$1
    
    echo -e "\n📝 运行测试: ${YELLOW}${test_func}${NC}"
    
    # 使用 Go 的内置超时机制运行测试
    go test -v -timeout ${TIMEOUT}s -run "^${test_func}$" . 2>&1 &
    local test_pid=$!
    
    # 等待测试完成或超时
    local start_time=$(date +%s)
    while kill -0 $test_pid 2>/dev/null; do
        local current_time=$(date +%s)
        local elapsed=$((current_time - start_time))
        
        if [ $elapsed -ge $TIMEOUT ]; then
            kill -9 $test_pid 2>/dev/null
            wait $test_pid 2>/dev/null
            echo -e "⏰ ${RED}TIMEOUT (可能死锁)${NC}: ${test_func}"
            timeout_tests+=("${test_func}")
            return
        fi
        
        sleep 1
    done
    
    # 获取测试退出状态
    wait $test_pid
    local exit_code=$?
    
    case $exit_code in
        0)
            echo -e "✅ ${GREEN}PASSED${NC}: ${test_func}"
            passed_tests+=("${test_func}")
            ;;
        *)
            echo -e "❌ ${RED}FAILED${NC}: ${test_func} (退出码: ${exit_code})"
            failed_tests+=("${test_func}")
            ;;
    esac
}

# 逐个运行测试
for test in "${tests[@]}"; do
    run_test "$test"
    
    # 每个测试之间稍微等待一下
    sleep 2
done

# 输出总结
echo -e "\n=========================="
echo -e "📊 ${YELLOW}测试结果总结${NC}"
echo -e "=========================="

echo -e "\n✅ ${GREEN}通过的测试 (${#passed_tests[@]})${NC}:"
for test in "${passed_tests[@]}"; do
    echo "   - $test"
done

echo -e "\n❌ ${RED}失败的测试 (${#failed_tests[@]})${NC}:"
for test in "${failed_tests[@]}"; do
    echo "   - $test"
done

echo -e "\n⏰ ${RED}超时的测试 (疑似死锁) (${#timeout_tests[@]})${NC}:"
for test in "${timeout_tests[@]}"; do
    echo "   - $test"
done

if [ ${#timeout_tests[@]} -gt 0 ]; then
    echo -e "\n🚨 ${RED}发现 ${#timeout_tests[@]} 个疑似死锁的测试函数！${NC}"
    echo -e "建议进一步分析这些函数的实现。"
    
    echo -e "\n🔍 可以单独深入分析超时的测试:"
    for test in "${timeout_tests[@]}"; do
        echo "   go test -v -timeout ${TIMEOUT}s -run '^${test}$'"
    done
else
    echo -e "\n🎉 ${GREEN}未发现明显的死锁问题！${NC}"
fi

echo -e "\n📝 如需更详细的分析，可以使用以下命令:"
echo "   go test -v -run '^函数名$' -timeout ${TIMEOUT}s"
echo "   go test -race -v -run '^函数名$' -timeout ${TIMEOUT}s  # 检测竞态条件" 