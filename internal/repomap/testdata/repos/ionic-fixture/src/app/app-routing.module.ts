import { NgModule } from '@angular/core';
import { RouterModule, Routes } from '@angular/router';

const routes: Routes = [
  { path: 'home', loadChildren: () => import('./home/home.page') },
  { path: 'profile', loadChildren: () => import('./profile/profile.page') },
  { path: 'apps', loadChildren: () => import('./apps/apps.page') },
];

@NgModule({ imports: [RouterModule.forRoot(routes)] })
export class AppRoutingModule {}
