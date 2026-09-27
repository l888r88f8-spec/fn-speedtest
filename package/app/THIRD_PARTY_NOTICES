# Third-Party Notices

## showwin/speedtest-go

牛速使用 `github.com/showwin/speedtest-go/speedtest` 与 Speedtest.net 节点交互。

The MIT License (MIT)

Copyright (c) 2015 ITO Shogo

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## IP geolocation service

牛速通过 HTTPS 调用 ipwho.is 查询 NAS 公网出口 IP 的运营商和省份。仅发送被查询的公网 IP；归属地结果在进程内按 IP 缓存，测速结果中的归属地随历史记录保存在 NAS。接口说明：https://ipwhois.io/documentation 。服务可用性和定位精度由提供方决定；接口不可达时继续使用 Speedtest 网络信息。

## Public HTTP testing services

v1.8.0 增加 Go 自行实现的 HTTP 测速引擎，读取公开 LibreSpeed 风格配置并使用其 HTTP 接口约定。协议参考：https://github.com/librespeed/speedtest 。未打包或复制 LibreSpeed 的 JavaScript/PHP 代码。

运营商和高校候选入口详见 README；节点通过 NAS 的小量下载、上传及延迟验证后才可选择。节点服务方可以看到 NAS 出口 IP 及测速请求；测速数据为随机／测试数据，不上传 NAS 用户文件。不会绕过登录或浏览器验证。Speedtest 元数据服务失效时，ipwho.is 也可通过一次 NAS 后端请求检测该请求的公网出口。

牛速与 Ookla/Speedtest.net、LibreSpeed、各运营商和高校均无官方隶属关系；站点名称仅用于标识服务来源。

## spiritLHLS/speedtest.cn-CN-ID

v1.10.0 在运行时读取 `https://github.com/spiritLHLS/speedtest.cn-CN-ID` 的中国大陆 Speedtest.cn 节点目录。目录数据按 MIT 许可证发布：

Copyright (c) 2023 spiritLHLS

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

目录许可证不代表 Speedtest.cn 或节点运营方对本应用的授权、认可或可用性保证。测速节点会看到 NAS 的公网出口 IP 和测试流量；不会上传 NAS 用户文件。牛速与 Speedtest.cn 及该目录维护者无官方隶属关系。

## GlobalSpeed protocol compatibility

v1.9.0 增加自行编写的 Go 协议适配，通过 taierspeed-cli 社区目录读取候选节点，并向节点申请单次测速会话。协议资料参考：

- https://github.com/ztelliot/taierspeed-cli （LGPL-3.0；README 另有使用说明）
- https://github.com/Gaimoydev/better-speedtest （MIT）

本包未分发上述两个项目的客户端或代码文件。适配器自行实现目录解析、HTTP 传输、有限数据探测、会话生命周期和错误处理；第三方软件许可证不代表其引用的网络服务提供方授权或保证可用。不会自动接受官方 App 用户协议。节点验证失败时跳过。

目录服务和测速节点可看到 NAS 出口 IP 及请求；会话请求使用一次性随机客户端标识，不收集硬件 IMEI，不上传 NAS 用户文件。具体接入范围和验证限制详见 README。牛速与中国信通院及该社区目录维护者无官方隶属关系。
