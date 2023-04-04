# 司南

是Tailscale的DERP的改版，目的在于适用于蜃境对于DERP的自动化部署（伪）与管理需求；   
整体设计思路在于保持蜃境对Tailscale官方的DERP（官方节点和自行部署）同样兼容的前提下，司南版本在命令行视角同样兼容Tailscale DERP的使用方式，但也可以被控制器管理，同时做到客户端连接司南时的认证（即Tailscale DERP的-verify-client参数）无需在司南节点启动其他程序、无需额外作为普通节点登录进蜃境网络。   


Thanks to Tailscale.
