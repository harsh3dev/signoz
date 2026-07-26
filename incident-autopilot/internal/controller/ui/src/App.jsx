import { Link, Outlet, useLocation } from 'react-router-dom';

const nav = [
  { to: '/', label: 'Dashboard' },
  { to: '/loadtest', label: 'Load Test' },
  { to: '/actions', label: 'Actions' },
];

export default function App() {
  const location = useLocation();

  return (
    <div className="min-h-screen bg-slate-950 text-slate-100 font-sans">
      <header className="border-b border-slate-800 bg-slate-900/80 backdrop-blur sticky top-0 z-10">
        <div className="max-w-6xl mx-auto px-6 py-4 flex items-center justify-between gap-6">
          <div>
            <h1 className="text-lg font-semibold tracking-tight">Incident Autopilot</h1>
            <p className="text-xs text-slate-400">Control plane</p>
          </div>
          <nav className="flex gap-1">
            {nav.map(({ to, label }) => {
              const active = location.pathname === to || (to !== '/' && location.pathname.startsWith(to));
              return (
                <Link
                  key={to}
                  to={to}
                  className={`px-4 py-2 rounded-lg text-sm font-medium transition-colors ${
                    active
                      ? 'bg-blue-600 text-white'
                      : 'text-slate-300 hover:bg-slate-800 hover:text-white'
                  }`}
                >
                  {label}
                </Link>
              );
            })}
          </nav>
        </div>
      </header>
      <main className="max-w-6xl mx-auto px-6 py-8">
        <Outlet />
      </main>
    </div>
  );
}
