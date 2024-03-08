## greatws task模块


## 设计目的
尽量让task 变成可插拔的模块，可以方便的扩展和替换以及测试第三方go程池的任务模块。

## io 模块
task 运行在io go程里面，适配轻量级任务，比如proxy，或者nginx，redis这种，基本不访问第三方中间件，比如mysql或者别的业务服务，只有数据包的加工

## stream 模块
task 会独占一个go程， 适合对数据包的加工有顺序要求的场景，比如asr识别这种

## unordered
task 只管并发执行，不管有序性，比如推送/红点/点赞之类的。