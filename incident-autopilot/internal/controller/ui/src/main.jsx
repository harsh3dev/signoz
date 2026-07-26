import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter, Route, Routes } from 'react-router-dom';
import './index.css';
import App from './App.jsx';
import Dashboard from './pages/Dashboard.jsx';
import LoadTest from './pages/LoadTest.jsx';
import Actions from './pages/Actions.jsx';

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<App />}>
          <Route index element={<Dashboard />} />
          <Route path="loadtest" element={<LoadTest />} />
          <Route path="actions" element={<Actions />} />
          <Route path="actions/:id" element={<Actions />} />
        </Route>
      </Routes>
    </BrowserRouter>
  </StrictMode>,
);
