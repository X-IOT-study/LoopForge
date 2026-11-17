document.getElementById('loginLang').onclick = () => window.lfSetLang(window.lfLang() === 'en' ? 'zh' : 'en');
document.getElementById('loginTheme').onclick = () => {
  const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
  document.documentElement.dataset.theme = next;
  localStorage.setItem('agentflow.theme.v3', next);
};

const $ = id => document.getElementById(id);
const t = window.lfT || ((zh) => zh);
let mode = 'login', signup = false, localMode = false;

// 本地模式来自 /api/auth/setup 的 localMode 字段，不看域名或 UA：同一份页面
// 在两种部署下只有这一处行为分叉，isLocalMode 为 false 时下面的分支全部走原逻辑。
function render() {
  const creating = mode !== 'login';
  const localSetup = localMode && mode === 'setup';
  const localLogin = localMode && mode === 'login';
  $('loginSubtitle').textContent = localSetup
    ? t('设置本地管理员密码', 'Set the local administrator password')
    : localLogin
      ? t('输入本地密码', 'Enter your local password')
      : mode === 'setup' ? t('创建管理员账号', 'Create the administrator account')
        : creating ? t('创建你的账号', 'Create your account')
          : t('登录你的交付工作台', 'Sign in to your delivery workspace');
  $('setupNotice').hidden = mode !== 'setup';
  if (localMode) {
    $('setupNotice').textContent = t(
      '这是本地模式的首次启动。设置密码后即可开始，之后用这个密码登录。',
      'First start in local mode. Set a password to begin; use it to sign in afterwards.'
    );
  }
  // 本地模式只保留「密码」一条路径：没有注册、切换账号、邀请或第三方登录入口。
  $('localModeNotice').hidden = !localMode;
  if (localMode) {
    $('localModeNotice').textContent = t(
      '本地模式账号：数据只保存在这台机器上，不连接云端服务。',
      'Local mode account: data stays on this machine and never reaches cloud services.'
    );
  }
  $('loginNameField').hidden = localMode;
  $('loginForm').elements.login.required = !localMode;
  $('confirmField').hidden = !creating;
  $('passwordRule').hidden = !creating;
  $('loginForm').elements.confirm.required = creating;
  $('loginForm').elements.password.autocomplete = creating ? 'new-password' : 'current-password';
  $('loginSubmit').textContent = localSetup
    ? t('设置密码并进入', 'Set password and continue')
    : localLogin ? t('登录', 'Sign in')
      : mode === 'setup' ? t('创建并进入', 'Create and continue')
        : creating ? t('注册并进入', 'Sign up') : t('登录', 'Sign in');
  $('loginSwitch').hidden = localMode || mode === 'setup' || !signup;
  $('switchText').textContent = creating ? t('已有账号？', 'Already have an account?') : t('还没有账号？', 'No account yet?');
  $('switchMode').textContent = creating ? t('登录', 'Sign in') : t('注册', 'Sign up');
  $('loginContact').hidden = localMode || mode === 'setup' || signup;
  $('loginError').textContent = '';
}

$('switchMode').onclick = () => { mode = mode === 'login' ? 'register' : 'login'; render(); };

fetch('/api/auth/setup').then(r => r.ok ? r.json() : {}).then(state => {
  localMode = !!state.localMode;
  signup = !!state.signupEnabled;
  if (state.needsSetup) mode = 'setup';
  else if (localMode) mode = 'login';
  else if (location.hash === '#register' && signup) mode = 'register';
  render();
}).catch(() => {});

$('loginForm').onsubmit = async event => {
  event.preventDefault();
  const form = event.currentTarget, button = $('loginSubmit'), error = $('loginError');
  const creating = mode !== 'login';
  if (creating && form.elements.password.value !== form.elements.confirm.value) {
    error.textContent = t('两次输入的密码不一致', 'Passwords do not match');
    return;
  }
  button.disabled = true;
  error.textContent = '';
  try {
    const response = await fetch(creating ? '/api/auth/register' : '/api/auth/password', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      // 本地模式的账号名固定为 local，服务端也只认密码；平台模式仍提交用户填写的登录名。
      body: JSON.stringify({login: localMode ? 'local' : form.elements.login.value.trim(), password: form.elements.password.value}),
    });
    let payload = {};
    try { payload = await response.json(); } catch (err) { payload = {}; }
    if (!response.ok) {
      // 未初始化时后端返回 409 local_mode_not_setup：直接切回设置密码，而不是笼统报错。
      if (payload.code === 'local_mode_not_setup') {
        mode = 'setup';
        render();
        error.textContent = t('本地管理员账号还没有设置密码，请先设置。', 'The local administrator has no password yet. Set one first.');
        button.disabled = false;
        return;
      }
      throw new Error(payload.error || t('操作失败', 'Request failed'));
    }
    location.replace('/');
  } catch (err) { error.textContent = err.message; button.disabled = false; }
};
