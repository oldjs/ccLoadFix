// 多标签页缓存同步
// 一个标签页改了渠道/令牌等数据，通过 BroadcastChannel 通知其他标签页刷新
(function(window) {
  var CHANNEL_NAME = 'ccload-cache-sync';
  var channel = null;

  try {
    if (typeof BroadcastChannel !== 'undefined') {
      channel = new BroadcastChannel(CHANNEL_NAME);
    }
  } catch (_) {
    // 浏览器不支持就降级为无同步，不影响功能
  }

  // 广播缓存失效事件给其他标签页
  function notify(type) {
    if (!channel) return;
    try {
      channel.postMessage({ type: type, ts: Date.now() });
    } catch (_) {}
  }

  // 注册监听：收到其他标签页的通知后执行回调
  function onInvalidate(type, callback) {
    if (!channel) return;
    channel.addEventListener('message', function(event) {
      if (event.data && event.data.type === type) {
        callback(event.data);
      }
    });
  }

  window.CacheSync = {
    notify: notify,
    onInvalidate: onInvalidate
  };
})(window);
