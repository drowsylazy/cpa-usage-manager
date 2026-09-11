// ---------- 启动 ----------
applyTheme();
$('held-auto').checked = localStorage.getItem('held-auto') === '1';
setupHeldAuto();
if (key) showApp();
else { $('gate').hidden = false; $('gate-key').focus(); }
})();
